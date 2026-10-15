package agent

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestReportsDoNotDelayUserRevocation(t *testing.T) {
	f := newFixture(t)
	f.reportGate = make(chan struct{})
	cfg := fixtureConfig(t, f)
	cfg.PullInterval = Duration(25 * time.Millisecond)
	cfg.RequestTimeout = Duration(2 * time.Second)
	a, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	defer func() {
		cancel()
		if e := <-done; e != nil {
			t.Error(e)
		}
	}()
	eventually(t, func() bool { return len(a.ListUsers()) == 2 })
	s := dialStream(t, a, f.users[0].UUID, echoServer(t), "", true)
	if e = exchange(s, []byte("live")); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	f.mu.Lock()
	f.users = []User{}
	f.mu.Unlock()
	eventually(t, func() bool { return len(a.ListUsers()) == 0 })
	if time.Since(start) > 750*time.Millisecond {
		t.Fatal("report request blocked user pull")
	}
	assertClosed(t, s)
}
func TestDownloadRateLimit(t *testing.T) {
	f := newFixture(t)
	f.users[0].SpeedLimit = 1
	a := newTestAgent(t, f)
	l, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer l.Close()
	payload := bytes.Repeat([]byte("d"), 128*1024)
	done := make(chan error, 1)
	go func() {
		c, e := l.Accept()
		if e != nil {
			done <- e
			return
		}
		defer c.Close()
		_, e = c.Write(payload)
		done <- e
	}()
	c := dialClient(t, a, f.users[0].UUID, l.Addr().String(), false)
	start := time.Now()
	got, e := io.ReadAll(c)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("download payload mismatch")
	}
	if e = <-done; e != nil {
		t.Fatal(e)
	}
	if time.Since(start) < 950*time.Millisecond {
		t.Fatalf("download rate exceeded 1 Mbps: %v", time.Since(start))
	}
}
func TestFailedListenerStopsAccessAndRebinds(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	target := echoServer(t)
	stream := dialStream(t, a, f.users[0].UUID, target, "", true)
	if err := exchange(stream, []byte("before listener failure")); err != nil {
		t.Fatal(err)
	}
	a.mu.Lock()
	listener := a.listener
	a.mu.Unlock()
	if err := listener.Listener.Close(); err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return len(a.ListUsers()) == 0 })
	assertClosed(t, stream)
	if err := a.pull(context.Background()); err != nil {
		t.Fatal(err)
	}
	fresh := dialClient(t, a, f.users[0].UUID, target, false)
	if err := exchange(fresh, []byte("after listener recovery")); err != nil {
		t.Fatal(err)
	}
}

func TestListenerConfigChangesAreApplied(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	target := echoServer(t)
	old := dialStream(t, a, f.users[0].UUID, target, "", true)
	if e := exchange(old, []byte("before")); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.config.ServerPort = freePort(t)
	f.config.Obfs = "http"
	f.mu.Unlock()
	if e := a.pull(context.Background()); e != nil {
		t.Fatal(e)
	}
	assertClosed(t, old)
	fresh := dialClient(t, a, f.users[0].UUID, target, false)
	if e := exchange(fresh, []byte("new binding and obfs")); e != nil {
		t.Fatal(e)
	}
}
func TestDeviceLimitReductionClosesExcessSources(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	target := echoServer(t)
	first := dialStream(t, a, f.users[0].UUID, target, "127.0.0.1", false)
	second := dialStream(t, a, f.users[0].UUID, target, "127.0.0.2", false)
	if e := exchange(first, []byte("one")); e != nil {
		t.Fatal(e)
	}
	if e := exchange(second, []byte("two")); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.users[0].DeviceLimit = 1
	f.mu.Unlock()
	if e := a.pull(context.Background()); e != nil {
		t.Fatal(e)
	}
	assertClosed(t, first)
	assertClosed(t, second)
}
