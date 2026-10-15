package xray

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/transport"
)

func openLifecycleLink(t testing.TB, d *LimitDispatcher, email, ip string) *closeTrackingWriter {
	t.Helper()
	lease, rejected := d.checkDeviceLimit(email, ip, true)
	if rejected {
		t.Fatalf("connection rejected: %s/%s", email, ip)
	}
	link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	d.trackLink(link, lease)
	return link.Writer.(*closeTrackingWriter)
}

func assertDispatcherEmpty(t testing.TB, d *LimitDispatcher) {
	t.Helper()
	users := 0
	d.unlimitedIPs.Range(func(_, _ any) bool { users++; return true })
	d.mu.RLock()
	limited := len(d.limitedIPs)
	d.mu.RUnlock()
	if users != 0 || limited != 0 || d.connCount.Load() != 0 {
		t.Fatalf("retained state: unlimited=%d limited=%d connections=%d", users, limited, d.connCount.Load())
	}
}

func TestLimitDispatcher_UnlimitedContainerChurn(t *testing.T) {
	d := newTestDispatcher()
	for uid := 1; uid <= 10000; uid++ {
		email := userEmail(uid)
		for ip := 0; ip < 4; ip++ {
			conn := openLifecycleLink(t, d, email, fmt.Sprintf("10.%d.%d.%d", uid>>8, uid&255, ip))
			conn.Close()
		}
	}
	assertDispatcherEmpty(t, d)
}

func TestLimitDispatcher_HotUpdateRelease(t *testing.T) {
	for _, sameIP := range []bool{false, true} {
		t.Run(fmt.Sprintf("sameIP=%v", sameIP), func(t *testing.T) {
			d := newTestDispatcher()
			email := userEmail(1)
			d.UpdateLimits(map[string]int{email: 1}, nil, nil)
			old := openLifecycleLink(t, d, email, "1.1.1.1")
			d.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 2}, nil)
			ip := "2.2.2.2"
			if sameIP {
				ip = "1.1.1.1"
			}
			limited := openLifecycleLink(t, d, email, ip)
			limited.Close()
			old.Close()
			assertDispatcherEmpty(t, d)
		})
	}
}

func TestLimitDispatcher_ResetLateClose(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprintf("limited=%v", limited), func(t *testing.T) {
			d := newTestDispatcher()
			email := userEmail(1)
			limits := map[string]int{}
			if limited {
				limits[email] = 1
			}
			d.UpdateLimits(map[string]int{email: 1}, limits, nil)
			old := openLifecycleLink(t, d, email, "1.1.1.1")
			d.ResetConns()
			current := openLifecycleLink(t, d, email, "1.1.1.1")
			old.Close()
			old.Interrupt()
			state, count := d.GetConnectionState()
			if count != 1 || !state[1]["1.1.1.1"] {
				t.Fatalf("late close changed new connection: state=%v count=%d", state, count)
			}
			current.Close()
			assertDispatcherEmpty(t, d)
		})
	}
}

func BenchmarkLimitDispatcherConnectionLifecycle(b *testing.B) {
	for _, limited := range []bool{false, true} {
		b.Run(fmt.Sprintf("limited=%v", limited), func(b *testing.B) {
			d := newTestDispatcher()
			email := userEmail(1)
			limits := map[string]int{}
			if limited {
				limits[email] = 2
			}
			d.UpdateLimits(map[string]int{email: 1}, limits, nil)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				openLifecycleLink(b, d, email, "1.1.1.1").Close()
			}
		})
	}
}

func TestLimitDispatcher_BidirectionalLimitSwitch(t *testing.T) {
	for _, limitedFirst := range []bool{false, true} {
		for _, oldFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("limitedFirst=%v/oldFirst=%v", limitedFirst, oldFirst), func(t *testing.T) {
				d := newTestDispatcher()
				email := userEmail(1)
				setLimit := func(limited bool) {
					limits := map[string]int{}
					if limited {
						limits[email] = 2
					}
					d.UpdateLimits(map[string]int{email: 1}, limits, nil)
				}
				setLimit(limitedFirst)
				old := openLifecycleLink(t, d, email, "1.1.1.1")
				setLimit(!limitedFirst)
				current := openLifecycleLink(t, d, email, "1.1.1.1")
				setLimit(limitedFirst)
				first, last := current, old
				if oldFirst {
					first, last = old, current
				}
				first.Interrupt()
				first.Close()
				state, count := d.GetConnectionState()
				if count != 1 || !state[1]["1.1.1.1"] {
					t.Fatalf("wrong surviving connection: %v/%d", state, count)
				}
				last.Close()
				assertDispatcherEmpty(t, d)
			})
		}
	}
}

func TestLimitDispatcher_LimitSwitchCountsExistingIPs(t *testing.T) {
	d := newTestDispatcher()
	email := userEmail(1)
	old := openLifecycleLink(t, d, email, "1.1.1.1")
	d.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	if lease, rejected := d.checkDeviceLimit(email, "2.2.2.2", true); !rejected {
		d.delConn(lease)
		t.Fatal("existing unlimited IP must participate in the new device gate")
	}
	same := openLifecycleLink(t, d, email, "1.1.1.1")
	old.Close()
	same.Close()
	assertDispatcherEmpty(t, d)
}

func TestLimitDispatcher_ConcurrentContainerRetirement(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprintf("limited=%v", limited), func(t *testing.T) {
			d := newTestDispatcher()
			users, limits := map[string]int{}, map[string]int{}
			for uid := 1; uid <= 8; uid++ {
				users[userEmail(uid)] = uid
				if limited {
					limits[userEmail(uid)] = 8
				}
			}
			d.UpdateLimits(users, limits, nil)
			var wg sync.WaitGroup
			start := make(chan struct{})
			held := make(chan *closeTrackingWriter, 64)
			for worker := 0; worker < 64; worker++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					<-start
					email := userEmail(worker%8 + 1)
					ip := fmt.Sprintf("10.0.0.%d", worker/8%4)
					for i := 0; i < 300; i++ {
						conn := openLifecycleLink(t, d, email, ip)
						conn.Close()
						conn.Interrupt()
					}
					held <- openLifecycleLink(t, d, email, ip)
				}()
			}
			close(start)
			wg.Wait()
			close(held)
			state, count := d.GetConnectionState()
			if count != 64 || len(state) != 8 {
				t.Errorf("lost active connections: users=%d connections=%d", len(state), count)
			}
			for uid := 1; uid <= 8; uid++ {
				if len(state[uid]) != 4 {
					t.Errorf("UID %d has %d IPs, want 4", uid, len(state[uid]))
				}
			}
			for conn := range held {
				conn.Close()
			}
			assertDispatcherEmpty(t, d)
		})
	}
}

func TestLimitDispatcher_ConcurrentSnapshotsUpdatesAndReset(t *testing.T) {
	d := newTestDispatcher()
	users, limits := map[string]int{}, map[string]int{}
	for uid := 1; uid <= 4; uid++ {
		users[userEmail(uid)] = uid
		limits[userEmail(uid)] = 16
	}
	d.UpdateLimits(users, limits, nil)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < 16; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 300; i++ {
				conn := openLifecycleLink(t, d, userEmail(worker%4+1), fmt.Sprintf("10.0.0.%d", worker%8))
				runtime.Gosched()
				conn.Interrupt()
				conn.Close()
			}
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 300; i++ {
			if i%2 == 0 {
				d.UpdateLimits(users, nil, nil)
			} else {
				d.UpdateLimits(users, limits, nil)
			}
			if i%3 == 0 {
				d.ResetConns()
			}
			runtime.Gosched()
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < 1000; i++ {
			d.GetConnectionState()
			runtime.Gosched()
		}
	}()
	close(start)
	wg.Wait()
	assertDispatcherEmpty(t, d)
}

func TestLimitDispatcher_UDPAdmission(t *testing.T) {
	d := newTestDispatcher()
	email := userEmail(1)
	udp, rejected := d.checkDeviceLimit(email, "1.1.1.1", false)
	if rejected {
		t.Fatal("unlimited UDP rejected")
	}
	d.delConn(udp)
	assertDispatcherEmpty(t, d)
	d.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	udp, rejected = d.checkDeviceLimit(email, "1.1.1.1", false)
	if rejected {
		t.Fatal("first UDP rejected")
	}
	d.delConn(udp)
	assertDispatcherEmpty(t, d)
	tcp := openLifecycleLink(t, d, email, "1.1.1.1")
	if _, rejected = d.checkDeviceLimit(email, "2.2.2.2", false); !rejected {
		t.Fatal("over-limit UDP allowed")
	}
	for _, ip := range []string{"1.1.1.1", "0.0.0.1"} {
		udp, rejected = d.checkDeviceLimit(email, ip, false)
		if rejected {
			t.Fatalf("eligible UDP rejected: %s", ip)
		}
		link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
		d.trackLink(link, udp)
		link.Writer.(*closeTrackingWriter).Close()
	}
	state, count := d.GetConnectionState()
	if count != 1 || len(state[1]) != 1 || !state[1]["1.1.1.1"] {
		t.Fatalf("UDP changed TCP state: %v/%d", state, count)
	}
	tcp.Close()
	assertDispatcherEmpty(t, d)
}

func lifecycleContext(email string) context.Context {
	return session.ContextWithInbound(context.Background(), &session.Inbound{
		Source: net.TCPDestination(net.LocalHostIP, 12345), User: &protocol.MemoryUser{Email: email},
	})
}

func TestLimitDispatcher_DisabledDeviceGate(t *testing.T) {
	d := newTestDispatcher()
	d.disableDeviceGate = true
	email := userEmail(1)
	d.UpdateLimits(map[string]int{email: 1}, map[string]int{email: 1}, nil)
	_, lease, err := d.identifyAndCheck(lifecycleContext(email), net.TCPDestination(net.LocalHostIP, 80))
	if err != nil {
		t.Fatal(err)
	}
	link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	d.trackLink(link, lease)
	state, count := d.GetConnectionState()
	if len(state) != 0 || count != 1 {
		t.Fatalf("disabled gate tracked device state: %v/%d", state, count)
	}
	d.ResetConns()
	current := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
	_, lease, err = d.identifyAndCheck(lifecycleContext(email), net.TCPDestination(net.LocalHostIP, 80))
	if err != nil {
		t.Fatal(err)
	}
	d.trackLink(current, lease)
	link.Writer.(*closeTrackingWriter).Close()
	if d.connCount.Load() != 1 {
		t.Fatal("disabled gate late close changed new count")
	}
	current.Writer.(*closeTrackingWriter).Close()
	assertDispatcherEmpty(t, d)
}

type failingLifecycleDispatcher struct {
	err              error
	closeBeforeError bool
}

func (*failingLifecycleDispatcher) Type() interface{} { return nil }
func (*failingLifecycleDispatcher) Start() error      { return nil }
func (*failingLifecycleDispatcher) Close() error      { return nil }
func (f *failingLifecycleDispatcher) Dispatch(context.Context, net.Destination) (*transport.Link, error) {
	return nil, f.err
}
func (f *failingLifecycleDispatcher) DispatchLink(_ context.Context, _ net.Destination, link *transport.Link) error {
	if f.closeBeforeError {
		link.Writer.(*closeTrackingWriter).Interrupt()
	}
	return f.err
}

func TestLimitDispatcher_DispatchFailureReleasesLease(t *testing.T) {
	for _, limited := range []bool{false, true} {
		for _, closeBeforeError := range []bool{false, true} {
			t.Run(fmt.Sprintf("limited=%v/closed=%v", limited, closeBeforeError), func(t *testing.T) {
				d := newTestDispatcher()
				email := userEmail(1)
				limits := map[string]int{}
				if limited {
					limits[email] = 1
				}
				d.UpdateLimits(map[string]int{email: 1}, limits, nil)
				want := errors.New("dispatch failed")
				d.innerDisp = &failingLifecycleDispatcher{err: want, closeBeforeError: closeBeforeError}
				ctx := lifecycleContext(email)
				dest := net.TCPDestination(net.LocalHostIP, 80)
				if _, err := d.Dispatch(ctx, dest); !errors.Is(err, want) {
					t.Fatalf("Dispatch error: %v", err)
				}
				assertDispatcherEmpty(t, d)
				link := &transport.Link{Reader: nopReader{}, Writer: buf.Discard}
				if err := d.DispatchLink(ctx, dest, link); !errors.Is(err, want) {
					t.Fatalf("DispatchLink error: %v", err)
				}
				assertDispatcherEmpty(t, d)
				link.Writer.(*closeTrackingWriter).Close()
				assertDispatcherEmpty(t, d)
			})
		}
	}
}

func TestLimitDispatcher_ActiveUserIPChurn(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprintf("limited=%v", limited), func(t *testing.T) {
			d := newTestDispatcher()
			email := userEmail(1)
			limits := map[string]int{}
			if limited {
				limits[email] = 2
			}
			d.UpdateLimits(map[string]int{email: 1}, limits, nil)
			anchor := openLifecycleLink(t, d, email, "0.0.0.1")
			for i := 0; i < 10000; i++ {
				openLifecycleLink(t, d, email, fmt.Sprintf("10.%d.%d.1", i>>8, i&255)).Close()
			}
			keys := 0
			if limited {
				d.mu.RLock()
				keys = len(d.limitedIPs[email])
				d.mu.RUnlock()
			} else {
				v, ok := d.unlimitedIPs.Load(email)
				if !ok {
					t.Fatal("active container disappeared")
				}
				v.(*ipCounter).ips.Range(func(_, _ any) bool { keys++; return true })
			}
			if keys != 1 {
				t.Fatalf("retained historical IP keys: %d, want 1", keys)
			}
			anchor.Close()
			assertDispatcherEmpty(t, d)
		})
	}
}

func TestLimitDispatcher_ConcurrentIdempotentRelease(t *testing.T) {
	for _, limited := range []bool{false, true} {
		t.Run(fmt.Sprintf("limited=%v", limited), func(t *testing.T) {
			d := newTestDispatcher()
			email := userEmail(1)
			limits := map[string]int{}
			if limited {
				limits[email] = 1
			}
			d.UpdateLimits(map[string]int{email: 1}, limits, nil)
			old := openLifecycleLink(t, d, email, "1.1.1.1")
			current := openLifecycleLink(t, d, email, "1.1.1.1")
			var wg sync.WaitGroup
			for i := 0; i < 64; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					if i%3 == 0 {
						old.Close()
					} else if i%3 == 1 {
						old.Interrupt()
					} else {
						old.onClose()
					}
				}()
			}
			wg.Wait()
			state, count := d.GetConnectionState()
			if count != 1 || !state[1]["1.1.1.1"] {
				t.Fatalf("repeated release changed surviving connection: %v/%d", state, count)
			}
			current.Close()
			newConn := openLifecycleLink(t, d, email, "1.1.1.1")
			old.onClose()
			current.onClose()
			state, count = d.GetConnectionState()
			if count != 1 || !state[1]["1.1.1.1"] {
				t.Fatalf("stale release changed recreated container: %v/%d", state, count)
			}
			newConn.Close()
			assertDispatcherEmpty(t, d)
		})
	}
}

func BenchmarkLimitDispatcherOverlappingConnections(b *testing.B) {
	for _, limited := range []bool{false, true} {
		b.Run(fmt.Sprintf("limited=%v", limited), func(b *testing.B) {
			d := newTestDispatcher()
			email := userEmail(1)
			limits := map[string]int{}
			if limited {
				limits[email] = 2
			}
			d.UpdateLimits(map[string]int{email: 1}, limits, nil)
			anchor := openLifecycleLink(b, d, email, "1.1.1.1")
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				openLifecycleLink(b, d, email, "1.1.1.1").Close()
			}
			b.StopTimer()
			anchor.Close()
		})
	}
}
