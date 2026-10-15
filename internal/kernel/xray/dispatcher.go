package xray

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
	_ "unsafe"

	xrayDispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/transport"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/nlog"
)

// Access xray's internal config creator registry so we can replace the
// default dispatcher factory with ours. This runs AFTER xray's init()
// functions because our package imports xray (dependency order guarantee).
//
//go:linkname typeCreatorRegistry github.com/xtls/xray-core/common.typeCreatorRegistry
var typeCreatorRegistry map[reflect.Type]common.ConfigCreator

var origDispatcherFactory common.ConfigCreator

// globalLimitDispatcher is set when the factory creates a LimitDispatcher.
// The Xray kernel reads it to configure limits and get connections.
var globalLimitDispatcher atomic.Pointer[LimitDispatcher]
var globalDisableXrayDeviceGate atomic.Bool

func init() {
	configType := reflect.TypeOf((*xrayDispatcher.Config)(nil))
	origDispatcherFactory = typeCreatorRegistry[configType]
	typeCreatorRegistry[configType] = limitDispatcherFactory
}

func limitDispatcherFactory(ctx context.Context, config interface{}) (interface{}, error) {
	orig, err := origDispatcherFactory(ctx, config)
	if err != nil {
		return nil, err
	}
	inner, ok := orig.(routing.Dispatcher)
	if !ok {
		return orig, nil
	}
	ld := &LimitDispatcher{
		inner:             orig,
		innerDisp:         inner,
		limitedIPs:        make(map[string]map[string]int),
		disableDeviceGate: globalDisableXrayDeviceGate.Load(),
	}
	if err := installAccessOutboundHook(ctx, config, orig); err != nil {
		return nil, err
	}
	globalLimitDispatcher.Store(ld)
	nlog.Core().Debug("xray: limit dispatcher installed")
	return ld, nil
}

// LimitDispatcher wraps xray's DefaultDispatcher to enforce per-user
// admission checks before a request is dispatched into xray-core.
//
// Dispatch retains the native pipe reader required by mux/XUDP close paths.
// Access collection decorates native writer counters and the routed outbound
// handoff; DispatchLink retains the core's own timeout/sniff reader wrappers.
type LimitDispatcher struct {
	inner     interface{}        // original DefaultDispatcher (Feature + Dispatcher)
	innerDisp routing.Dispatcher // same object, typed as Dispatcher
	access    atomic.Pointer[access.Collector]

	// limitedUsers: users with device limit > 0, protected by mu.
	// Needs deterministic IP ordering for kick decisions.
	mu           sync.RWMutex
	limitedIPs   map[string]map[string]int // email → sourceIP → refcount
	deviceLimits map[string]int            // email → max devices
	emailToUID   map[string]int            // email → panel user ID

	// unlimitedIPs: per-user containers; membership changes under their own lock.
	// Each entry is *ipCounter{ips sync.Map}.
	unlimitedIPs sync.Map // email → *ipCounter

	connCount         atomic.Int64 // total active connections tracked by dispatcher
	disableDeviceGate bool
	connEpoch         uint64 // protected by mu; invalidates leases on reset
}

// ipCounter serializes admission and retirement for a single unlimited user.
type ipCounter struct {
	mu      sync.Mutex
	refs    int
	retired bool
	ips     sync.Map // sourceIP -> *atomic.Int64 (refcount)
}

// connectionLease owns the exact admission state, independent of later limits.
type connectionLease struct {
	email, sourceIP string
	epoch           uint64
	limited         map[string]int
	unlimited       *ipCounter
	tracked         bool // protected by dispatcher.mu
	closed          atomic.Bool
}

// aliveIPs returns a snapshot of distinct IPs.
func (ic *ipCounter) aliveIPs() map[string]bool {
	result := make(map[string]bool)
	ic.ips.Range(func(key, _ interface{}) bool {
		if rv, ok := ic.ips.Load(key); ok && rv.(*atomic.Int64).Load() > 0 {
			result[key.(string)] = true
		}
		return true
	})
	return result
}

// ─── routing.Dispatcher ──────────────────────────────────────────────────────

func (d *LimitDispatcher) Dispatch(ctx context.Context, dest net.Destination) (*transport.Link, error) {
	email, lease, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return nil, err
	}

	ctx, flow := d.accessContext(ctx, dest, email)
	link, err := d.innerDisp.Dispatch(ctx, dest)
	if err != nil {
		d.delConn(lease)
		return nil, err
	}

	if flow != nil {
		link.Writer = wrapAccessWriter(link.Writer, flow, destinationTarget(dest), true)
	}
	if email != "" {
		d.trackLink(link, lease)
	}
	return link, nil
}

func (d *LimitDispatcher) DispatchLink(ctx context.Context, dest net.Destination, link *transport.Link) error {
	email, lease, err := d.identifyAndCheck(ctx, dest)
	if err != nil {
		return err
	}

	ctx, flow := d.accessContext(ctx, dest, email)
	if flow != nil {
		// Native DispatchLink always puts its own TimeoutWrapperReader around
		// this supplied reader. Dispatch/mux pipe readers are never replaced.
		link.Reader = &accessReader{Reader: link.Reader, flow: flow, target: destinationTarget(dest)}
	}
	if email != "" {
		d.trackLink(link, lease)
	}
	err = d.innerDisp.DispatchLink(ctx, dest, link)
	if err != nil {
		d.delConn(lease)
	}
	return err
}

// identifyAndCheck extracts identity and binds lifecycle to admission state.
func (d *LimitDispatcher) identifyAndCheck(ctx context.Context, dest net.Destination) (email string, lease *connectionLease, err error) {
	si := session.InboundFromContext(ctx)
	if si == nil || si.User == nil || len(si.User.Email) == 0 {
		return "", nil, nil
	}
	email = si.User.Email
	if d.disableDeviceGate {
		d.mu.RLock()
		lease = &connectionLease{epoch: d.connEpoch}
		d.mu.RUnlock()
		return email, lease, nil
	}
	sourceIP := si.Source.Address.IP().String()
	lease, rejected := d.checkDeviceLimit(email, sourceIP, dest.Network == net.Network_TCP)
	if rejected {
		nlog.Core().Debug("xray: device limit exceeded", "email", email, "ip", sourceIP)
		return "", nil, errors.New("device limit exceeded for " + email)
	}
	return email, lease, nil
}

// trackLink leaves core-owned readers intact for mux/XUDP close paths.
func (d *LimitDispatcher) trackLink(link *transport.Link, lease *connectionLease) {
	d.mu.Lock()
	if lease.epoch == d.connEpoch && !lease.closed.Load() && !lease.tracked {
		lease.tracked = true
		d.connCount.Add(1)
	}
	d.mu.Unlock()

	link.Writer = &closeTrackingWriter{
		Writer:  link.Writer,
		onClose: func() { d.delConn(lease) },
	}
}

// ─── features.Feature (delegated) ───────────────────────────────────────────

func (d *LimitDispatcher) Type() interface{} { return routing.DispatcherType() }

func (d *LimitDispatcher) Start() error {
	if s, ok := d.inner.(interface{ Start() error }); ok {
		return s.Start()
	}
	return nil
}

func (d *LimitDispatcher) Close() error {
	if c, ok := d.inner.(interface{ Close() error }); ok {
		return c.Close()
	}
	return nil
}

// ─── Limit management (called by Xray kernel) ──────────────────────────────

func (d *LimitDispatcher) UpdateLimits(emailToUID map[string]int, deviceLimits, _ map[string]int) {
	d.mu.Lock()
	d.emailToUID = emailToUID
	d.deviceLimits = deviceLimits
	d.mu.Unlock()

}

func (d *LimitDispatcher) ResetConns() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.connEpoch++
	d.limitedIPs = make(map[string]map[string]int)
	d.unlimitedIPs.Clear()
	d.connCount.Store(0)
}

// GetConnectionState returns dispatcher-tracked alive IPs and connection count.
// Traffic bytes are intentionally left to xray's built-in stats pipeline.
func (d *LimitDispatcher) GetConnectionState() (aliveIPs map[int]map[string]bool, connCount int) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	emailToUID := d.emailToUID
	limitedIPs := d.limitedIPs

	aliveIPs = make(map[int]map[string]bool)

	// Collect IPs from limited users (under RLock snapshot).
	for email, ipsMap := range limitedIPs {
		uid := emailToUID[email]
		if uid == 0 {
			continue
		}
		ipSet := make(map[string]bool, len(ipsMap))
		for ip := range ipsMap {
			ipSet[ip] = true
		}
		if len(ipSet) > 0 {
			aliveIPs[uid] = ipSet
		}
	}

	// Collect unlimited IPs while reset and the UID mapping remain stable.
	d.unlimitedIPs.Range(func(key, value interface{}) bool {
		email := key.(string)
		uid := emailToUID[email]
		if uid == 0 {
			return true
		}
		ic := value.(*ipCounter)
		if ips := ic.aliveIPs(); len(ips) > 0 {
			// Merge with limited IPs if any
			if existing, ok := aliveIPs[uid]; ok {
				for ip := range ips {
					existing[ip] = true
				}
			} else {
				aliveIPs[uid] = ips
			}
		}
		return true
	})

	connCount = int(d.connCount.Load())
	return
}

// ─── Internal helpers ───────────────────────────────────────────────────────

// checkDeviceLimit returns a lease for the actual admission container.
func (d *LimitDispatcher) checkDeviceLimit(email, sourceIP string, isTCP bool) (*connectionLease, bool) {
	for {
		d.mu.RLock()
		limit := d.deviceLimits[email]
		if limit <= 0 {
			lease := &connectionLease{email: email, sourceIP: sourceIP, epoch: d.connEpoch}
			if isTCP {
				for {
					v, ok := d.unlimitedIPs.Load(email)
					if !ok {
						v, _ = d.unlimitedIPs.LoadOrStore(email, &ipCounter{})
					}
					ic := v.(*ipCounter)
					ic.mu.Lock()
					if ic.retired {
						ic.mu.Unlock()
						continue
					}
					rv, ok := ic.ips.Load(sourceIP)
					if !ok {
						rv = &atomic.Int64{}
						ic.ips.Store(sourceIP, rv)
					}
					rv.(*atomic.Int64).Add(1)
					ic.refs++
					lease.unlimited = ic
					ic.mu.Unlock()
					break
				}
			}
			d.mu.RUnlock()
			return lease, false
		}
		d.mu.RUnlock()

		// Check and register atomically; limits may change during lock upgrade.
		d.mu.Lock()
		limit = d.deviceLimits[email]
		if limit <= 0 {
			d.mu.Unlock()
			continue
		}
		ips := d.limitedIPs[email]
		active := ips
		if v, ok := d.unlimitedIPs.Load(email); ok {
			active = make(map[string]int, len(ips))
			for ip, count := range ips {
				active[ip] = count
			}
			for ip := range v.(*ipCounter).aliveIPs() {
				active[ip]++
			}
		}
		if active[sourceIP] <= 0 && len(active) >= limit {
			// Preserve the original lexicographic admission policy.
			ipList := make([]string, 0, len(active)+1)
			for ip := range active {
				ipList = append(ipList, ip)
			}
			ipList = append(ipList, sourceIP)
			sort.Strings(ipList)
			allowed := false
			for _, ip := range ipList[:limit] {
				if ip == sourceIP {
					allowed = true
					break
				}
			}
			if !allowed {
				d.mu.Unlock()
				return nil, true
			}
		}
		lease := &connectionLease{email: email, sourceIP: sourceIP, epoch: d.connEpoch}
		if isTCP {
			if ips == nil {
				ips = make(map[string]int)
				d.limitedIPs[email] = ips
			}
			ips[sourceIP]++
			lease.limited = ips
		}
		d.mu.Unlock()
		return lease, false
	}
}

// delConn releases a lease once, without consulting the current device limit.
func (d *LimitDispatcher) delConn(lease *connectionLease) {
	if lease == nil || !lease.closed.CompareAndSwap(false, true) {
		return
	}
	if lease.limited != nil {
		d.mu.Lock()
		defer d.mu.Unlock()
	} else {
		d.mu.RLock()
		defer d.mu.RUnlock()
	}
	if lease.epoch != d.connEpoch {
		return
	}
	if ic := lease.unlimited; ic != nil {
		ic.mu.Lock()
		if rv, ok := ic.ips.Load(lease.sourceIP); ok {
			if rv.(*atomic.Int64).Add(-1) == 0 {
				ic.ips.Delete(lease.sourceIP)
			}
		}
		ic.refs--
		if ic.refs == 0 {
			// Admissions that already loaded this container must retry.
			ic.retired = true
			d.unlimitedIPs.CompareAndDelete(lease.email, ic)
		}
		ic.mu.Unlock()
	}
	if ips := lease.limited; ips != nil {
		ips[lease.sourceIP]--
		if ips[lease.sourceIP] == 0 {
			delete(ips, lease.sourceIP)
		}
		if len(ips) == 0 {
			delete(d.limitedIPs, lease.email)
		}
	}
	if lease.tracked {
		d.connCount.Add(-1)
	}
}

type closeTrackingWriter struct {
	buf.Writer
	onClose func()
	closed  atomic.Bool
}

func (w *closeTrackingWriter) Close() error {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	return common.Close(w.Writer)
}

func (w *closeTrackingWriter) Interrupt() {
	if w.closed.CompareAndSwap(false, true) {
		w.onClose()
	}
	common.Interrupt(w.Writer)
}
