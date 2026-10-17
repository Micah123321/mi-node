package singbox

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"runtime"
	"sync"
	"testing"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	"github.com/sagernet/sing/common/bufio"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func accessTotals(s access.Snapshot) (u, d int64) {
	for _, e := range s.Entries {
		u += e.Upload
		d += e.Download
	}
	return
}
func accessMetadata() adapter.InboundContext {
	m := testInboundContext("uuid-1", "198.51.100.99")
	m.Destination = singM.Socksaddr{Fqdn: "Video.Example.", Port: 443}
	return m
}
func accessTracker() *ConnTracker {
	ct := NewConnTracker(0)
	ct.SetUserMap(map[string]int{"uuid-1": 123})
	ct.access.Configure(access.Config{Enabled: true})
	return ct
}

type accessCopyConn struct {
	testConn
	reader io.Reader
}

func (c *accessCopyConn) Read(b []byte) (int, error) { return c.reader.Read(b) }

type accessPacketConn struct {
	net.Conn
	dests      []singM.Socksaddr
	packets    [][]byte
	writeError error
}

func (c *accessPacketConn) ReadPacket(b *buf.Buffer) (singM.Socksaddr, error) {
	if len(c.packets) == 0 {
		return singM.Socksaddr{}, io.EOF
	}
	b.Write(c.packets[0])
	dest := c.dests[0]
	c.packets = c.packets[1:]
	c.dests = c.dests[1:]
	return dest, nil
}
func (c *accessPacketConn) WritePacket(b *buf.Buffer, _ singM.Socksaddr) error {
	b.Release()
	return c.writeError
}
func (c *accessPacketConn) Close() error { return nil }

func TestAccessTCPDirectAndRealOptimizedCopyCountOnce(t *testing.T) {
	ct := accessTracker()
	base := &accessCopyConn{reader: bytes.NewReader([]byte("hello"))}
	conn := ct.RoutedConnection(context.Background(), base, accessMetadata(), nil, nil)
	if _, err := conn.Read(make([]byte, 5)); err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("bye"))
	base.reader = bytes.NewReader([]byte("optimized-up"))
	n, err := bufio.Copy(io.Discard, conn)
	if n != 12 || err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("upload copy %d %v", n, err)
	}
	n, err = bufio.Copy(conn, bytes.NewReader([]byte("optimized-down")))
	if n != 14 || err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("download copy %d %v", n, err)
	}
	s := ct.access.Drain()
	if u, d := accessTotals(s); u != 17 || d != 17 {
		t.Fatalf("access bytes %d/%d", u, d)
	}
	if len(s.Entries) != 1 || s.Entries[0].Domain != "video.example" || s.Entries[0].UserID != 123 || s.Entries[0].Connections != 1 || s.Entries[0].IP != "" {
		t.Fatalf("metadata %+v", s)
	}
	if raw, _, _ := ct.GetUserTraffic(); raw[123] != [2]int64{17, 17} {
		t.Fatalf("billing changed %+v", raw)
	}
	conn.Close()
	if u, d := accessTotals(ct.access.Drain()); u != 0 || d != 0 {
		t.Fatal("close duplicated tail")
	}
}

func TestAccessUDPPerPacketAndUnwrapUnknown(t *testing.T) {
	ct := accessTracker()
	domain := singM.Socksaddr{Fqdn: "dns.example", Port: 53}
	ip := singM.Socksaddr{Addr: netip.MustParseAddr("192.0.2.8"), Port: 443}
	base := &accessPacketConn{dests: []singM.Socksaddr{domain, ip}, packets: [][]byte{[]byte("abc"), []byte("12345")}}
	conn := ct.RoutedPacketConnection(context.Background(), base, accessMetadata(), nil, nil)
	for i := 0; i < 2; i++ {
		b := buf.NewPacket()
		if _, err := conn.ReadPacket(b); err != nil {
			t.Fatal(err)
		}
		b.Release()
	}
	b := buf.As([]byte("down"))
	if err := conn.WritePacket(b, ip); err != nil {
		t.Fatal(err)
	}
	s := ct.access.Drain()
	if len(s.Entries) != 2 {
		t.Fatalf("packet destinations %+v", s)
	}
	for _, e := range s.Entries {
		switch {
		case e.Domain == "dns.example":
			if e.Upload != 3 || e.Download != 0 || e.DomainSource != "request" {
				t.Fatalf("domain %+v", e)
			}
		case e.IP == "192.0.2.8":
			if e.Upload != 5 || e.Download != 4 || e.DomainSource != "ip" {
				t.Fatalf("IP %+v", e)
			}
		default:
			t.Fatalf("unexpected destination %+v", e)
		}
	}
	_, read := N.UnwrapCountPacketReader(conn, nil)
	_, write := N.UnwrapCountPacketWriter(conn, nil)
	for _, fn := range read {
		fn(7)
	}
	for _, fn := range write {
		fn(11)
	}
	unknown := ct.access.Drain()
	if len(unknown.Entries) != 1 || unknown.Entries[0].Domain != "" || unknown.Entries[0].IP != "" || unknown.Entries[0].Port != 0 || unknown.Entries[0].DomainSource != "unknown" {
		t.Fatalf("unwrap guessed destination %+v", unknown)
	}
	if u, d := accessTotals(unknown); u != 7 || d != 11 {
		t.Fatalf("unwrap bytes %d/%d", u, d)
	}
	conn.Close()
}

func TestAccessUDPActualOptimizedCopy(t *testing.T) {
	ct := accessTracker()
	addr := singM.Socksaddr{Fqdn: "udp.example", Port: 53}
	source := &accessPacketConn{dests: []singM.Socksaddr{addr}, packets: [][]byte{[]byte("packet")}}
	trackedSource := ct.RoutedPacketConnection(context.Background(), source, accessMetadata(), nil, nil)
	sink := &accessPacketConn{}
	trackedSink := ct.RoutedPacketConnection(context.Background(), sink, accessMetadata(), nil, nil)
	n, err := bufio.CopyPacket(trackedSink, trackedSource)
	if n != 6 || !errors.Is(err, io.EOF) {
		t.Fatalf("packet copy %d %v", n, err)
	}
	s := ct.access.Drain()
	if u, d := accessTotals(s); u != 6 || d != 6 {
		t.Fatalf("optimized packet count %d/%d", u, d)
	}
	if len(s.Entries) != 1 || s.Entries[0].DomainSource != "unknown" || s.Entries[0].Domain != "" {
		t.Fatalf("optimized packet guessed %+v", s)
	}
	// Preserve the pre-existing sing upstream billing path: read counter runs,
	// writer is unwrapped before its billing/limiter callback is consulted.
	if raw, _, _ := ct.GetUserTraffic(); raw[123] != [2]int64{6, 0} {
		t.Fatalf("upstream billing behavior changed %+v", raw)
	}
	trackedSource.Close()
	trackedSink.Close()
}

func TestAccessTargetUsesOnlyProvenSniffMetadata(t *testing.T) {
	m := accessMetadata()
	m.Destination = singM.Socksaddr{Addr: netip.MustParseAddr("192.0.2.1"), Port: 443}
	m.Domain = "reverse.example"
	if target := accessTarget(m, "tcp"); target.Domain != "" || target.IP != "192.0.2.1" {
		t.Fatalf("reverse mapping used %+v", target)
	}
	m.Protocol = "tls"
	m.Domain = "sni.example"
	target := accessTarget(m, "tcp").Normalize()
	if target.Domain != "sni.example" || target.DomainSource != "sniff" || target.IP != "192.0.2.1" {
		t.Fatalf("SNI %+v", target)
	}
	m.SniffError = errors.New("failed sniff")
	if target := accessTarget(m, "tcp"); target.Domain != "" {
		t.Fatalf("failed sniff %+v", target)
	}
}

func TestAccessSharedThroughRealSingBoxRestartReloadAndDisable(t *testing.T) {
	s := New(config.KernelConfig{Type: "sing-box", ConfigDir: t.TempDir()})
	s.ConfigureAccess(access.Config{Enabled: true})
	users := []model.UserSpec{{ID: 123, UUID: "11111111-1111-1111-1111-111111111111"}}
	nc := trafficTestNode(t)
	if err := s.Start(nc, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	m := accessMetadata()
	m.User = users[0].UUID
	conn := s.connTracker.RoutedConnection(context.Background(), &testConn{}, m, nil, nil)
	conn.Write([]byte("first"))
	if _, d := accessTotals(s.DrainAccess()); d != 5 {
		t.Fatal("live connection not sliced")
	}
	old := s.connTracker.access
	if err := s.Reload(nc, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	if err := s.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	if s.connTracker.access != old {
		t.Fatal("collector replaced")
	}
	conn.Write([]byte("tail"))
	conn.Close()
	s.recycleWG.Wait()
	s.ConfigureAccess(access.Config{})
	if _, d := accessTotals(s.DrainAccess()); d != 4 {
		t.Fatal("disabled tail lost")
	}
	conn.Write([]byte("disabled"))
	if len(s.DrainAccess().Entries) != 0 {
		t.Fatal("disabled collector active")
	}
}

type accessConcurrentConn struct{ net.Conn }

func (*accessConcurrentConn) Write(b []byte) (int, error) { return len(b), nil }
func (*accessConcurrentConn) Close() error                { return nil }
func TestAccessTargetIgnoresQUICListenerAndPreservesOriginalRequest(t *testing.T) {
	for _, port := range []uint16{0, 30443} {
		m := accessMetadata()
		m.OriginDestination = singM.Socksaddr{Addr: netip.MustParseAddr("127.0.0.1"), Port: port}
		if got := accessTarget(m, "tcp"); got.Domain != "Video.Example." || got.Port != 443 {
			t.Fatalf("listener replaced request: %+v", got)
		}
		m.RouteOriginalDestination = m.Destination
		m.Destination = singM.Socksaddr{Addr: netip.MustParseAddr("192.0.2.5"), Port: 8443}
		if got := accessTarget(m, "tcp"); got.Domain != "Video.Example." || got.Port != 443 {
			t.Fatalf("route override replaced original request: %+v", got)
		}
	}
	m := accessMetadata()
	m.FakeIP = true
	m.OriginDestination = singM.Socksaddr{Addr: netip.MustParseAddr("198.18.0.1"), Port: 443}
	if got := accessTarget(m, "tcp"); got.IP != "198.18.0.1" || got.Port != 443 || got.Domain != "" {
		t.Fatalf("fake-IP original destination was lost: %+v", got)
	}
}

func TestAccessConcurrentDrainAndClose(t *testing.T) {
	ct := accessTracker()
	conn := ct.RoutedConnection(context.Background(), &accessConcurrentConn{}, accessMetadata(), nil, nil)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			conn.Write([]byte("abc"))
		}
	}()
	go func() { defer wg.Done(); conn.Close() }()
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	var total int64
	for {
		select {
		case <-done:
			_, d := accessTotals(ct.access.Drain())
			total += d
			if total != 3000 {
				t.Fatalf("concurrent drain %d", total)
			}
			return
		default:
			_, d := accessTotals(ct.access.Drain())
			total += d
			runtime.Gosched()
		}
	}
}
