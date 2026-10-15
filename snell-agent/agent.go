package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/missuo/opensnell/components/snell"
)

type userState struct {
	spec     User
	name     string
	devices  map[string]int
	up, down pacer
}
type authView struct {
	users   []snell.User
	expires time.Time
}
type Agent struct {
	cfg            Config
	panel          *panelClient
	logger         *slog.Logger
	queue          *reportQueue
	stats          trafficStats
	ctx            context.Context
	cancel         context.CancelFunc
	pullMu         sync.Mutex
	reportMu       sync.Mutex
	mu             sync.Mutex
	users          map[int64]*userState
	names          map[string]*userState
	authUsers      []snell.User
	auth           atomic.Pointer[authView]
	generation     uint64
	conns          map[string]*transportConn
	lastPull       time.Time
	suspended      bool
	storageOK      bool
	closed         bool
	listening      bool
	panelConfig    PanelConfig
	listener       *trackedListener
	listenerCancel context.CancelFunc
	listenerDone   chan struct{}
	closeOnce      sync.Once
	closeErr       error
}

func New(c Config, logger *slog.Logger) (*Agent, error) {
	if err := c.normalize(); err != nil {
		return nil, err
	}
	q, err := openQueue(c)
	if err != nil {
		return nil, err
	}
	if logger == nil {
		logger = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &Agent{cfg: c, panel: newPanel(c), logger: logger, queue: q, stats: trafficStats{traffic: map[string][2]int64{}}, ctx: ctx, cancel: cancel, users: map[int64]*userState{}, names: map[string]*userState{}, conns: map[string]*transportConn{}, storageOK: true}
	a.auth.Store(&authView{})
	return a, nil
}
func (a *Agent) ListUsers() []snell.User {
	v := a.auth.Load()
	if v == nil || !time.Now().Before(v.expires) {
		return nil
	}
	return v.users
}
func (a *Agent) readyLocked() bool {
	return !a.closed && a.listening && !a.suspended && a.storageOK && len(a.users) > 0 && !a.lastPull.IsZero() && time.Since(a.lastPull) < time.Duration(a.cfg.StaleAfter)
}
func (a *Agent) publishLocked() {
	v := &authView{}
	if a.readyLocked() {
		v.users = a.authUsers
		v.expires = a.lastPull.Add(time.Duration(a.cfg.StaleAfter))
	}
	a.auth.Store(v)
}
func (a *Agent) connectionsLocked() []*transportConn {
	out := make([]*transportConn, 0, len(a.conns))
	for _, c := range a.conns {
		out = append(out, c)
	}
	return out
}
func closeTransports(cs []*transportConn) {
	for _, c := range cs {
		_ = c.Close()
	}
}
func (a *Agent) setStorage(ok bool) {
	a.mu.Lock()
	a.storageOK = ok
	a.publishLocked()
	var cs []*transportConn
	if !ok {
		cs = a.connectionsLocked()
	}
	a.mu.Unlock()
	closeTransports(cs)
}
func (a *Agent) expire() {
	a.mu.Lock()
	var cs []*transportConn
	if !a.readyLocked() {
		a.publishLocked()
		cs = a.connectionsLocked()
	}
	a.mu.Unlock()
	closeTransports(cs)
}
func (a *Agent) watchdog(ctx context.Context) {
	interval := time.Duration(a.cfg.StaleAfter) / 4
	if interval > 250*time.Millisecond {
		interval = 250 * time.Millisecond
	}
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			a.expire()
		}
	}
}
func (a *Agent) stopListener() {
	a.mu.Lock()
	ln, cancel, done := a.listener, a.listenerCancel, a.listenerDone
	a.listener = nil
	a.listenerCancel = nil
	a.listenerDone = nil
	a.listening = false
	a.publishLocked()
	cs := a.connectionsLocked()
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if ln != nil {
		_ = ln.Close()
	}
	closeTransports(cs)
	if done != nil {
		<-done
	}
}
func (a *Agent) startListener(p PanelConfig) error {
	server, err := snell.NewServer(snell.ServerConfig{Listen: a.cfg.address(p), UserStore: a, ObfsMode: p.Obfs, UDP: false, QUIC: false, IPv6: true, DialTimeout: 10 * time.Second, OnAuthorize: a.authorize}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		return errors.New("initialize Snell server failed")
	}
	raw, err := net.Listen("tcp", a.cfg.address(p))
	if err != nil {
		return errors.New("bind Snell TCP listener failed")
	}
	ln := &trackedListener{Listener: raw, agent: a}
	ctx, cancel := context.WithCancel(a.ctx)
	done := make(chan struct{})
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		cancel()
		raw.Close()
		return net.ErrClosed
	}
	a.listener = ln
	a.listening = true
	a.listenerCancel = cancel
	a.listenerDone = done
	a.mu.Unlock()
	go func() {
		defer close(done)
		if err := server.ServeListener(ctx, ln); err != nil && ctx.Err() == nil {
			a.mu.Lock()
			var cs []*transportConn
			if a.listener == ln {
				a.listening = false
				a.publishLocked()
				cs = a.connectionsLocked()
			}
			a.mu.Unlock()
			closeTransports(cs)
			a.logger.Error("Snell listener stopped")
		}
	}()
	return nil
}
func (a *Agent) pull(ctx context.Context) error {
	a.pullMu.Lock()
	defer a.pullMu.Unlock()
	p, err := a.panel.config(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	a.suspended = p.suspended()
	a.publishLocked()
	var cs []*transportConn
	if a.suspended {
		cs = a.connectionsLocked()
	}
	a.mu.Unlock()
	closeTransports(cs)
	users, err := a.panel.users(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	rebind := a.listener == nil || !a.listening || a.cfg.address(p) != a.cfg.address(a.panelConfig) || p.Obfs != a.panelConfig.Obfs
	a.mu.Unlock()
	if rebind {
		a.stopListener()
		if err = a.startListener(p); err != nil {
			return err
		}
	}
	a.mu.Lock()
	next := make(map[int64]*userState, len(users))
	names := make(map[string]*userState, len(users))
	auth := make([]snell.User, 0, len(users))
	for _, u := range users {
		state := a.users[u.ID]
		if state == nil || state.spec.UUID != u.UUID {
			a.generation++
			state = &userState{name: fmt.Sprintf("%d:%d", u.ID, a.generation), devices: map[string]int{}}
		}
		state.spec = u
		state.up.set(u.SpeedLimit)
		state.down.set(u.SpeedLimit)
		next[u.ID] = state
		names[state.name] = state
		auth = append(auth, snell.User{Name: state.name, PSK: []byte(u.UUID)})
	}
	cs = nil
	for _, t := range a.conns {
		if len(next) == 0 || (t.user != nil && (next[t.user.spec.ID] != t.user || (t.user.spec.DeviceLimit > 0 && len(t.user.devices) > t.user.spec.DeviceLimit))) {
			cs = append(cs, t)
		}
	}
	a.users = next
	a.names = names
	a.authUsers = auth
	a.panelConfig = p
	a.lastPull = time.Now()
	a.publishLocked()
	a.mu.Unlock()
	closeTransports(cs)
	return nil
}
func sourceIP(addr net.Addr) (string, error) {
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return "", errors.New("invalid source address")
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "", errors.New("invalid source IP")
	}
	return ip.Unmap().String(), nil
}
func (a *Agent) authorize(ctx context.Context, info snell.ConnContext, dialer snell.ContextDialer) (net.Conn, error) {
	a.mu.Lock()
	state := a.names[info.User]
	t := a.conns[info.RemoteAddr.String()]
	valid := a.readyLocked() && state != nil && t != nil && !t.closed && (t.user == nil || t.user == state)
	if valid && t.user == nil {
		if state.spec.DeviceLimit > 0 && state.devices[t.ip] == 0 && len(state.devices) >= state.spec.DeviceLimit {
			valid = false
		} else {
			t.user = state
			state.devices[t.ip]++
		}
	}
	if !valid {
		a.mu.Unlock()
		if t != nil {
			t.Close()
		}
		return nil, errors.New("access denied")
	}
	_ = t.Conn.SetReadDeadline(time.Time{})
	a.mu.Unlock()
	upstream, err := dialer.DialContext(t.ctx, "tcp", info.Target)
	if err != nil {
		return nil, errors.New("destination connection failed")
	}
	a.mu.Lock()
	if !a.readyLocked() || t.closed || a.names[info.User] != state {
		a.mu.Unlock()
		upstream.Close()
		t.Close()
		return nil, errors.New("access revoked")
	}
	mctx, cancel := context.WithCancel(t.ctx)
	m := &meteredConn{Conn: upstream, agent: a, transport: t, user: state, uid: strconv.FormatInt(state.spec.ID, 10), ctx: mctx, cancel: cancel}
	t.upstreams[m] = struct{}{}
	a.mu.Unlock()
	return m, nil
}
func (a *Agent) snapshotReport() (Report, error) {
	id, err := reportID()
	if err != nil {
		return Report{}, err
	}
	r := Report{ID: id, Alive: map[string][]string{}, Online: map[string]int{}}
	a.mu.Lock()
	defer a.mu.Unlock()
	r.Metrics = Metrics{KernelStatus: a.readyLocked(), Users: len(a.users), Connections: len(a.conns)}
	for uid, u := range a.users {
		key := strconv.FormatInt(uid, 10)
		ips := make([]string, 0, len(u.devices))
		for ip := range u.devices {
			ips = append(ips, ip)
		}
		sort.Strings(ips)
		r.Alive[key] = ips
		r.Online[key] = len(ips)
	}
	return r, nil
}
func (a *Agent) checkpoint() error {
	r, err := a.snapshotReport()
	if err != nil {
		return err
	}
	return a.stats.checkpoint(a.queue, r)
}
func (a *Agent) report(ctx context.Context) error {
	a.reportMu.Lock()
	defer a.reportMu.Unlock()
	if err := a.checkpoint(); err != nil {
		a.setStorage(false)
		return err
	}
	a.setStorage(true)
	// Bound a recovery pass so polling can still advance through a long backlog.
	for i := 0; i < 16; i++ {
		r, ok := a.queue.first()
		if !ok {
			return nil
		}
		if err := a.panel.report(ctx, r); err != nil {
			return err
		}
		if err := a.queue.ack(r.ID); err != nil {
			a.setStorage(false)
			return err
		}
	}
	return nil
}
func (a *Agent) intervals() (time.Duration, time.Duration) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.interval(a.cfg.PullInterval, a.panelConfig.BaseConfig.PullInterval, 30*time.Second), a.cfg.interval(a.cfg.ReportInterval, a.panelConfig.BaseConfig.PushInterval, 60*time.Second)
}
func (a *Agent) Run(ctx context.Context) (result error) {
	defer func() { result = errors.Join(result, a.Close()) }()
	runCtx, cancel := context.WithCancel(a.ctx)
	defer cancel()
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-runCtx.Done():
		}
	}()
	var workers sync.WaitGroup
	workers.Add(3)
	go func() { defer workers.Done(); a.watchdog(runCtx) }()
	go func() {
		defer workers.Done()
		for runCtx.Err() == nil {
			if err := a.pull(runCtx); err != nil && runCtx.Err() == nil {
				a.logger.Warn("panel pull failed", "reason", err)
			}
			p, _ := a.intervals()
			if !waitInterval(runCtx, p) {
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for runCtx.Err() == nil {
			if err := a.report(runCtx); err != nil && runCtx.Err() == nil {
				a.logger.Warn("panel report failed", "reason", err)
			}
			_, r := a.intervals()
			if !waitInterval(runCtx, r) {
				return
			}
		}
	}()
	<-runCtx.Done()
	workers.Wait()
	return nil
}
func waitInterval(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
func (a *Agent) Close() error {
	a.closeOnce.Do(func() {
		a.cancel()
		a.pullMu.Lock()
		defer a.pullMu.Unlock()
		a.reportMu.Lock()
		defer a.reportMu.Unlock()
		a.mu.Lock()
		a.closed = true
		a.publishLocked()
		a.mu.Unlock()
		a.stopListener()
		a.closeErr = a.checkpoint()
		a.panel.http.CloseIdleConnections()
		a.closeErr = errors.Join(a.closeErr, a.queue.close())
	})
	return a.closeErr
}

type trackedListener struct {
	net.Listener
	agent *Agent
}

func (l *trackedListener) Accept() (net.Conn, error) {
	for {
		raw, err := l.Listener.Accept()
		if err != nil {
			return nil, err
		}
		ip, err := sourceIP(raw.RemoteAddr())
		if err != nil {
			raw.Close()
			continue
		}
		a := l.agent
		a.mu.Lock()
		if !a.readyLocked() || a.listener != l || len(a.conns) >= 8192 {
			a.mu.Unlock()
			raw.Close()
			continue
		}
		ctx, cancel := context.WithCancel(a.ctx)
		t := &transportConn{Conn: raw, agent: a, ip: ip, ctx: ctx, cancel: cancel, upstreams: map[*meteredConn]struct{}{}}
		a.conns[raw.RemoteAddr().String()] = t
		_ = raw.SetReadDeadline(time.Now().Add(15 * time.Second))
		a.mu.Unlock()
		return t, nil
	}
}

type transportConn struct {
	net.Conn
	agent     *Agent
	ip        string
	ctx       context.Context
	cancel    context.CancelFunc
	user      *userState
	upstreams map[*meteredConn]struct{}
	closed    bool
	once      sync.Once
}

func (t *transportConn) Close() error {
	t.once.Do(func() {
		a := t.agent
		a.mu.Lock()
		t.closed = true
		if a.conns[t.RemoteAddr().String()] == t {
			delete(a.conns, t.RemoteAddr().String())
		}
		if t.user != nil {
			t.user.devices[t.ip]--
			if t.user.devices[t.ip] <= 0 {
				delete(t.user.devices, t.ip)
			}
		}
		ups := make([]*meteredConn, 0, len(t.upstreams))
		for m := range t.upstreams {
			ups = append(ups, m)
		}
		a.mu.Unlock()
		t.cancel()
		_ = t.Conn.Close()
		for _, m := range ups {
			m.Close()
		}
	})
	return nil
}
