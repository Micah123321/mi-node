package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/missuo/opensnell/components/obfs"
	"github.com/missuo/opensnell/components/snell"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

type panelFixture struct {
	mu         sync.Mutex
	config     PanelConfig
	users      []User
	userBody   string
	userStatus int
	configBody string
	failures   int
	ack        string
	requests   []Report
	bodies     [][]byte
	accepted   map[string]bool
	totals     map[string][2]int64
	server     *httptest.Server
	reportGate <-chan struct{}
}

func newFixture(t *testing.T) *panelFixture {
	t.Helper()
	port := freePort(t)
	f := &panelFixture{config: PanelConfig{Protocol: "snell", ListenIP: "127.0.0.1", ServerPort: port, Version: 4, Obfs: "off"}, users: []User{{1, "user-one-raw-psk", 0, 0}, {2, "user-two-raw-psk", 0, 0}}, accepted: map[string]bool{}, totals: map[string][2]int64{}}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.server.Close)
	return f
}
func (f *panelFixture) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("token") != "fixture-token" || r.URL.Query().Get("node_id") != "42" {
		http.Error(w, "invalid auth", 401)
		return
	}
	if strings.HasSuffix(r.URL.Path, "/report") && f.reportGate != nil {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		select {
		case <-r.Context().Done():
			return
		case <-f.reportGate:
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/api/v2/server/config":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		if f.configBody != "" {
			io.WriteString(w, f.configBody)
			return
		}
		json.NewEncoder(w).Encode(f.config)
	case "/api/v2/server/user":
		if r.Method != "GET" {
			w.WriteHeader(405)
			return
		}
		if f.userStatus != 0 {
			w.WriteHeader(f.userStatus)
			return
		}
		if f.userBody != "" {
			io.WriteString(w, f.userBody)
			return
		}
		json.NewEncoder(w).Encode(struct {
			Users []User `json:"users"`
		}{f.users})
	case "/api/v2/server/report":
		if r.Method != "POST" {
			w.WriteHeader(405)
			return
		}
		b, _ := io.ReadAll(r.Body)
		var report Report
		if decodeStrict(b, &report) != nil {
			w.WriteHeader(422)
			return
		}
		f.requests = append(f.requests, report)
		f.bodies = append(f.bodies, b)
		if !f.accepted[report.ID] {
			f.accepted[report.ID] = true
			for uid, v := range report.Traffic {
				sum := f.totals[uid]
				sum[0] += v[0]
				sum[1] += v[1]
				f.totals[uid] = sum
			}
		}
		// Simulate the panel committing traffic then losing its acknowledgement.
		if f.failures > 0 {
			f.failures--
			w.WriteHeader(503)
			return
		}
		if f.ack != "" {
			io.WriteString(w, f.ack)
			return
		}
		io.WriteString(w, `{"data":true}`)
	default:
		w.WriteHeader(404)
	}
}
func freePort(t *testing.T) int {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	return port
}
func fixtureConfig(t *testing.T, f *panelFixture) Config {
	return Config{PanelURL: f.server.URL, Token: "fixture-token", NodeID: 42, StateDir: t.TempDir(), StaleAfter: Duration(time.Minute), RequestTimeout: Duration(time.Second)}
}
func newTestAgent(t *testing.T, f *panelFixture) *Agent {
	t.Helper()
	a, e := New(fixtureConfig(t, f), quiet)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() {
		if e := a.Close(); e != nil {
			t.Error(e)
		}
	})
	if e = a.pull(context.Background()); e != nil {
		t.Fatal(e)
	}
	return a
}
func echoServer(t *testing.T) string {
	t.Helper()
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	var mu sync.Mutex
	conns := map[net.Conn]bool{}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, e := l.Accept()
			if e != nil {
				return
			}
			mu.Lock()
			conns[c] = true
			mu.Unlock()
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				io.Copy(c, c)
				mu.Lock()
				delete(conns, c)
				mu.Unlock()
			}()
		}
	}()
	t.Cleanup(func() {
		l.Close()
		mu.Lock()
		for c := range conns {
			c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return l.Addr().String()
}
func splitTarget(t *testing.T, target string) (string, uint16) {
	t.Helper()
	host, port, e := net.SplitHostPort(target)
	if e != nil {
		t.Fatal(e)
	}
	n, e := strconv.ParseUint(port, 10, 16)
	if e != nil {
		t.Fatal(e)
	}
	return host, uint16(n)
}
func dialClient(t *testing.T, a *Agent, key, target string, reuse bool) net.Conn {
	t.Helper()
	a.mu.Lock()
	addr := a.cfg.address(a.panelConfig)
	mode := a.panelConfig.Obfs
	a.mu.Unlock()
	c, e := snell.NewClient(snell.ClientConfig{Server: addr, PSK: key, Version: "v4", ObfsMode: mode, Reuse: reuse, DialTimeout: time.Second}, quiet)
	if e != nil {
		t.Fatal(e)
	}
	host, port := splitTarget(t, target)
	conn, e := c.DialTCP(context.Background(), host, port)
	if e != nil {
		t.Fatal(e)
	}
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	t.Cleanup(func() { conn.Close() })
	return conn
}
func dialStream(t *testing.T, a *Agent, key, target, source string, reuse bool) *snell.Snell {
	t.Helper()
	a.mu.Lock()
	addr := a.cfg.address(a.panelConfig)
	mode := a.panelConfig.Obfs
	a.mu.Unlock()
	d := net.Dialer{Timeout: time.Second}
	if source != "" {
		d.LocalAddr = &net.TCPAddr{IP: net.ParseIP(source)}
	}
	raw, e := d.DialContext(context.Background(), "tcp", addr)
	if e != nil {
		t.Fatal(e)
	}
	raw.SetDeadline(time.Now().Add(10 * time.Second))
	_, portText, _ := net.SplitHostPort(addr)
	ob, e := obfs.NewObfsClient(raw, "localhost", portText, mode)
	if e != nil {
		t.Fatal(e)
	}
	s := snell.StreamConn(ob, []byte(key))
	host, port := splitTarget(t, target)
	if e = snell.WriteHeader(s, host, port, reuse); e != nil {
		s.Close()
		t.Fatal(e)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
func exchange(c net.Conn, data []byte) error {
	if _, e := c.Write(data); e != nil {
		return e
	}
	got := make([]byte, len(data))
	if _, e := io.ReadFull(c, got); e != nil {
		return e
	}
	if !bytes.Equal(data, got) {
		return errors.New("echo mismatch")
	}
	return nil
}
func eventually(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition did not converge")
}
func assertClosed(t *testing.T, s *snell.Snell) {
	t.Helper()
	s.Conn.SetReadDeadline(time.Now().Add(time.Second))
	_, e := s.Conn.Read(make([]byte, 16))
	if e == nil {
		t.Fatal("revoked transport still readable")
	}
	if ne, ok := e.(net.Error); ok && ne.Timeout() {
		t.Fatal("revoked transport remained open until timeout")
	}
}
func endReuse(t *testing.T, s *snell.Snell) {
	t.Helper()
	if e := snell.HalfClose(s); e != nil {
		t.Fatal(e)
	}
	_, e := s.Conn.Read(make([]byte, 16))
	if !errors.Is(e, snell.ErrZeroChunk) {
		t.Fatalf("reuse drain: %v", e)
	}
}

func TestSnellTwoUsersAndHTTP(t *testing.T) {
	for _, mode := range []string{"off", "http"} {
		t.Run(mode, func(t *testing.T) {
			f := newFixture(t)
			f.config.Obfs = mode
			a := newTestAgent(t, f)
			target := echoServer(t)
			c1 := dialClient(t, a, f.users[0].UUID, target, false)
			c2 := dialClient(t, a, f.users[1].UUID, target, false)
			if e := exchange(c1, bytes.Repeat([]byte("a"), 12345)); e != nil {
				t.Fatal(e)
			}
			if e := exchange(c2, bytes.Repeat([]byte("b"), 777)); e != nil {
				t.Fatal(e)
			}
			request := "GET /local HTTP/1.1\r\nHost: fixture\r\nConnection: close\r\n\r\n"
			response := "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello"
			l, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer l.Close()
			done := make(chan error, 1)
			go func() {
				c, e := l.Accept()
				if e != nil {
					done <- e
					return
				}
				defer c.Close()
				b := make([]byte, len(request))
				_, e = io.ReadFull(c, b)
				if e == nil && string(b) != request {
					e = errors.New("HTTP request mismatch")
				}
				if e == nil {
					_, e = io.WriteString(c, response)
				}
				done <- e
			}()
			h := dialClient(t, a, f.users[0].UUID, l.Addr().String(), false)
			if _, e = io.WriteString(h, request); e != nil {
				t.Fatal(e)
			}
			got, e := io.ReadAll(h)
			if e != nil {
				t.Fatal(e)
			}
			if string(got) != response {
				t.Fatalf("HTTP mismatch: %q", got)
			}
			if e = <-done; e != nil {
				t.Fatal(e)
			}
			c1.Close()
			c2.Close()
			h.Close()
			a.expire()
			eventually(t, func() bool {
				a.stats.mu.Lock()
				defer a.stats.mu.Unlock()
				return a.stats.traffic["1"] == [2]int64{12345 + int64(len(request)), 12345 + int64(len(response))} && a.stats.traffic["2"] == [2]int64{777, 777}
			})
			if e = a.report(context.Background()); e != nil {
				t.Fatal(e)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.totals["1"] != [2]int64{12345 + int64(len(request)), 12345 + int64(len(response))} || f.totals["2"] != [2]int64{777, 777} {
				t.Fatalf("wrong accounting: %v", f.totals)
			}
			if !f.requests[0].Metrics.KernelStatus || f.requests[0].Metrics.UDP || f.requests[0].Metrics.QUIC {
				t.Fatal("wrong kernel metrics")
			}
		})
	}
}
func TestReuseMeteringAndAuthorizeEveryRequest(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	target := echoServer(t)
	s := dialStream(t, a, f.users[0].UUID, target, "", true)
	if e := exchange(s, []byte("first")); e != nil {
		t.Fatal(e)
	}
	endReuse(t, s)
	host, port := splitTarget(t, target)
	if e := snell.WriteHeader(s, host, port, true); e != nil {
		t.Fatal(e)
	}
	if e := exchange(s, []byte("second-longer")); e != nil {
		t.Fatal(e)
	}
	endReuse(t, s)
	if e := a.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	if f.totals["1"] != [2]int64{18, 18} {
		t.Errorf("reused accounting: %v", f.totals)
	}
	f.users = []User{}
	f.mu.Unlock()
	if e := a.pull(context.Background()); e != nil {
		t.Fatal(e)
	}
	assertClosed(t, s)
}
func TestDeletionAndKeyRotationCloseActiveAndIdle(t *testing.T) {
	for _, revoke := range []string{"delete", "rotate", "delete-readd"} {
		for _, idle := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/idle=%t", revoke, idle), func(t *testing.T) {
				f := newFixture(t)
				a := newTestAgent(t, f)
				target := echoServer(t)
				old := f.users[0].UUID
				s := dialStream(t, a, old, target, "", true)
				if e := exchange(s, []byte("before")); e != nil {
					t.Fatal(e)
				}
				if idle {
					endReuse(t, s)
				}
				other := dialClient(t, a, f.users[1].UUID, target, false)
				if e := exchange(other, []byte("other")); e != nil {
					t.Fatal(e)
				}
				f.mu.Lock()
				if revoke == "rotate" {
					f.users[0].UUID = "new-key-raw-UTF8-测试"
				} else {
					f.users = f.users[1:]
				}
				f.mu.Unlock()
				if e := a.pull(context.Background()); e != nil {
					t.Fatal(e)
				}
				assertClosed(t, s)
				if e := exchange(other, []byte("still-live")); e != nil {
					t.Fatalf("other user was revoked: %v", e)
				}
				if revoke == "delete-readd" {
					f.mu.Lock()
					f.users = append(f.users, User{ID: 1, UUID: old})
					f.mu.Unlock()
					if e := a.pull(context.Background()); e != nil {
						t.Fatal(e)
					}
					assertClosed(t, s)
					fresh := dialClient(t, a, old, target, false)
					if e := exchange(fresh, []byte("fresh")); e != nil {
						t.Fatal(e)
					}
				} else {
					denied := dialStream(t, a, old, target, "", false)
					denied.SetDeadline(time.Now().Add(time.Second))
					if e := exchange(denied, []byte("denied")); e == nil {
						t.Fatal("old key still accepted")
					}
				}
				if revoke == "rotate" {
					fresh := dialClient(t, a, "new-key-raw-UTF8-测试", target, false)
					if e := exchange(fresh, []byte("rotated")); e != nil {
						t.Fatal(e)
					}
				}
			})
		}
	}
}
func TestDeviceLimitUsesDistinctSourceIPsIncludingIdleReuse(t *testing.T) {
	f := newFixture(t)
	f.users[0].DeviceLimit = 1
	a := newTestAgent(t, f)
	target := echoServer(t)
	key := f.users[0].UUID
	one := dialStream(t, a, key, target, "127.0.0.1", true)
	if e := exchange(one, []byte("one")); e != nil {
		t.Fatal(e)
	}
	endReuse(t, one)
	same := dialStream(t, a, key, target, "127.0.0.1", false)
	if e := exchange(same, []byte("same IP")); e != nil {
		t.Fatal(e)
	}
	denied := dialStream(t, a, key, target, "127.0.0.2", false)
	denied.SetDeadline(time.Now().Add(time.Second))
	if e := exchange(denied, []byte("different IP")); e == nil {
		t.Fatal("device limit bypassed")
	}
	r, e := a.snapshotReport()
	if e != nil {
		t.Fatal(e)
	}
	if r.Online["1"] != 1 || len(r.Alive["1"]) != 1 || r.Alive["1"][0] != "127.0.0.1" {
		t.Fatalf("wrong alive: %+v", r)
	}
	one.Close()
	same.Close()
	eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.users[1].devices) == 0 })
	allowed := dialStream(t, a, key, target, "127.0.0.2", false)
	if e := exchange(allowed, []byte("slot released")); e != nil {
		t.Fatal(e)
	}
}
func TestSpeedLimitAggregatesConcurrentConnections(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	target := echoServer(t)
	// Changing a limit preserves the authorized connection and updates its shared budget.
	c1 := dialClient(t, a, f.users[0].UUID, target, false)
	c2 := dialClient(t, a, f.users[0].UUID, target, false)
	if e := exchange(c1, []byte("pre")); e != nil {
		t.Fatal(e)
	}
	if e := exchange(c2, []byte("pre")); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.users[0].SpeedLimit = 1
	f.mu.Unlock()
	if e := a.pull(context.Background()); e != nil {
		t.Fatal(e)
	}
	data := bytes.Repeat([]byte("x"), 96*1024)
	start := time.Now()
	ch := make(chan error, 2)
	go func() { ch <- exchange(c1, data) }()
	go func() { ch <- exchange(c2, data) }()
	for i := 0; i < 2; i++ {
		if e := <-ch; e != nil {
			t.Fatal(e)
		}
	}
	elapsed := time.Since(start)
	if elapsed < 1450*time.Millisecond {
		t.Fatalf("per-user 1 Mbps aggregate exceeded: %v", elapsed)
	}
	fast := dialClient(t, a, f.users[1].UUID, target, false)
	start = time.Now()
	if e := exchange(fast, []byte("unlimited")); e != nil {
		t.Fatal(e)
	}
	if time.Since(start) > time.Second {
		t.Fatal("another user shared rate budget")
	}
}
func TestInvalidSnapshotsPreserveValidUsers(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	target := echoServer(t)
	s := dialClient(t, a, f.users[0].UUID, target, false)
	if e := exchange(s, []byte("initial")); e != nil {
		t.Fatal(e)
	}
	original := a.lastPull
	bad := []string{`{}`, `{"users":null}`, `{"users":{}}`, `{"users":[null]}`, `{"users":[{"id":1,"uuid":"x","speed_limit":-1,"device_limit":0}]}`, `{"users":[{"id":0,"uuid":"x","speed_limit":0,"device_limit":0}]}`, `{"users":[{"id":1,"uuid":"x"}]}`, `{"users":[{"id":1,"uuid":"x","speed_limit":0,"device_limit":0},{"id":2,"uuid":"x","speed_limit":0,"device_limit":0}]}`, `{"users":[{"id":1,"uuid":"x","speed_limit":0,"device_limit":0},{"id":1,"uuid":"y","speed_limit":0,"device_limit":0}]}`, `{"users":[]} {}`}
	for _, body := range bad {
		f.mu.Lock()
		f.userBody = body
		f.mu.Unlock()
		if e := a.pull(context.Background()); e == nil {
			t.Fatalf("accepted malformed snapshot: %s", body)
		}
		if !a.lastPull.Equal(original) || len(a.ListUsers()) != 2 {
			t.Fatal("invalid snapshot replaced valid users")
		}
		if e := exchange(s, []byte("preserved")); e != nil {
			t.Fatal(e)
		}
	}
	f.mu.Lock()
	f.userBody = ""
	f.userStatus = 500
	f.mu.Unlock()
	if a.pull(context.Background()) == nil {
		t.Fatal("accepted HTTP error")
	}
	if !a.lastPull.Equal(original) {
		t.Fatal("HTTP error renewed snapshot")
	}
	f.mu.Lock()
	f.userStatus = 0
	f.users = []User{}
	f.mu.Unlock()
	if e := a.pull(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(a.ListUsers()) != 0 {
		t.Fatal("empty users did not deny access")
	}
	if e := exchange(s, []byte("revoked")); e == nil {
		t.Fatal("empty snapshot did not close old connection")
	}
}
func TestStaleSuspensionAndUnsupportedConfig(t *testing.T) {
	t.Run("stale", func(t *testing.T) {
		f := newFixture(t)
		cfg := fixtureConfig(t, f)
		cfg.StaleAfter = Duration(120 * time.Millisecond)
		a, e := New(cfg, quiet)
		if e != nil {
			t.Fatal(e)
		}
		defer a.Close()
		if e = a.pull(context.Background()); e != nil {
			t.Fatal(e)
		}
		target := echoServer(t)
		s := dialStream(t, a, f.users[0].UUID, target, "", true)
		if e = exchange(s, []byte("fresh")); e != nil {
			t.Fatal(e)
		}
		endReuse(t, s)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go a.watchdog(ctx)
		f.mu.Lock()
		f.userBody = `{"users":null}`
		f.mu.Unlock()
		if a.pull(context.Background()) == nil {
			t.Fatal("invalid users accepted")
		}
		eventually(t, func() bool { a.mu.Lock(); defer a.mu.Unlock(); return len(a.conns) == 0 })
		assertClosed(t, s)
		if len(a.ListUsers()) != 0 {
			t.Fatal("stale auth remains enabled")
		}
	})
	t.Run("suspended", func(t *testing.T) {
		f := newFixture(t)
		a := newTestAgent(t, f)
		target := echoServer(t)
		s := dialStream(t, a, f.users[0].UUID, target, "", true)
		if e := exchange(s, []byte("live")); e != nil {
			t.Fatal(e)
		}
		f.mu.Lock()
		f.config.TrafficLimit.Suspended = true
		f.userStatus = 500
		f.mu.Unlock()
		if a.pull(context.Background()) == nil {
			t.Fatal("expected user error")
		}
		assertClosed(t, s)
		if len(a.ListUsers()) != 0 {
			t.Fatal("suspended node authenticates")
		}
		f.mu.Lock()
		f.config.TrafficLimit.Suspended = false
		f.config.TrafficLimit.Status = "suspended"
		f.userStatus = 0
		f.mu.Unlock()
		if e := a.pull(context.Background()); e != nil {
			t.Fatal(e)
		}
		if len(a.ListUsers()) != 0 {
			t.Fatal("status suspension ignored")
		}
		f.mu.Lock()
		f.config.TrafficLimit.Status = "normal"
		f.mu.Unlock()
		if e := a.pull(context.Background()); e != nil {
			t.Fatal(e)
		}
		s2 := dialClient(t, a, f.users[0].UUID, target, false)
		if e := exchange(s2, []byte("resumed")); e != nil {
			t.Fatal(e)
		}
	})
	for _, field := range []string{`"udp":true`, `"quic":true`, `"version":5`, `"obfs":"tls"`, `"obfs_host":"ignored.example"`, `"routes":[{"action":"reject"}]`, `"outbounds":[{"type":"socks"}]`} {
		t.Run(field, func(t *testing.T) {
			f := newFixture(t)
			a := newTestAgent(t, f)
			f.mu.Lock()
			b, _ := json.Marshal(f.config)
			var raw map[string]json.RawMessage
			json.Unmarshal(b, &raw)
			var replace map[string]json.RawMessage
			json.Unmarshal([]byte("{"+field+"}"), &replace)
			for k, v := range replace {
				raw[k] = v
			}
			b, _ = json.Marshal(raw)
			f.configBody = string(b)
			f.mu.Unlock()
			if a.pull(context.Background()) == nil {
				t.Fatal("unsupported config was accepted")
			}
		})
	}
}
func TestUDPAndQUICDisabled(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	addr := a.cfg.address(a.panelConfig)
	udp, e := net.ListenPacket("udp", addr)
	if e != nil {
		t.Fatalf("UDP port unexpectedly occupied: %v", e)
	}
	udp.Close()
	c, e := snell.NewClient(snell.ClientConfig{Server: addr, PSK: f.users[0].UUID, Version: "v4"}, quiet)
	if e != nil {
		t.Fatal(e)
	}
	done := make(chan error, 1)
	go func() {
		pc, e := c.DialUDP(context.Background())
		if pc != nil {
			pc.Close()
		}
		done <- e
	}()
	select {
	case e = <-done:
		if e == nil {
			t.Fatal("UDP association accepted")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("UDP rejection stalled")
	}
}
func TestCredentialErrorsAreRedacted(t *testing.T) {
	p := newPanel(Config{PanelURL: "http://127.0.0.1:1", Token: "secret-$-token", NodeID: 42, RequestTimeout: Duration(100 * time.Millisecond)})
	_, e := p.users(context.Background())
	if e == nil || strings.Contains(e.Error(), "secret") || strings.Contains(e.Error(), "token=") {
		t.Fatalf("credential leak: %v", e)
	}
}
