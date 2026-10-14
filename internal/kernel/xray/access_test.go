package xray

import (
	"bytes"
	"context"
	"io"
	gonet "net"
	"sync"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/model"
	dispatcher "github.com/xtls/xray-core/app/dispatcher"
	appstats "github.com/xtls/xray-core/app/stats"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/transport"
	"github.com/xtls/xray-core/transport/pipe"
	"golang.org/x/time/rate"
)

func accessTotals(s access.Snapshot) (u, d int64) {
	for _, e := range s.Entries {
		u += e.Upload
		d += e.Download
	}
	return
}
func accessSession() context.Context {
	return session.ContextWithInbound(context.Background(), &session.Inbound{Source: net.TCPDestination(net.LocalHostIP, 12345), User: &protocol.MemoryUser{Email: userEmail(123)}})
}
func accessKernel(t *testing.T) *Xray {
	t.Helper()
	x := New(config.KernelConfig{Type: "xray", ConfigDir: t.TempDir()})
	x.ConfigureAccess(access.Config{Enabled: true})
	users := []model.UserSpec{{ID: 123, UUID: "11111111-1111-1111-1111-111111111111"}}
	if err := x.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.Stop)
	return x
}

func tcpAccessEcho(t *testing.T, n int) net.Destination {
	t.Helper()
	listener, err := gonet.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		conn.SetDeadline(time.Now().Add(5 * time.Second))
		b := make([]byte, n)
		if _, err := io.ReadFull(conn, b); err == nil {
			conn.Write(b)
		}
	}()
	return net.TCPDestination(net.LocalHostIP, net.Port(listener.Addr().(*gonet.TCPAddr).Port))
}
func udpAccessEcho(t *testing.T) net.Destination {
	t.Helper()
	conn, err := gonet.ListenUDP("udp", &gonet.UDPAddr{IP: gonet.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go func() {
		b := make([]byte, 2048)
		for {
			n, addr, err := conn.ReadFromUDP(b)
			if err != nil {
				return
			}
			conn.WriteToUDP(b[:n], addr)
		}
	}()
	return net.UDPDestination(net.LocalHostIP, net.Port(conn.LocalAddr().(*gonet.UDPAddr).Port))
}

func TestAccessNativeXrayDispatchPreservesPipeAndCountsDirections(t *testing.T) {
	for _, network := range []string{"tcp", "udp"} {
		t.Run(network, func(t *testing.T) {
			x := accessKernel(t)
			payload := []byte("native-xray")
			var dest net.Destination
			if network == "tcp" {
				dest = tcpAccessEcho(t, len(payload))
			} else {
				dest = udpAccessEcho(t)
			}
			ctx, cancel := context.WithCancel(session.SetForcedOutboundTagToContext(context.WithValue(accessSession(), core.XrayKey(1), x.instance), "direct"))
			defer cancel()
			link, err := x.limitDispatcher.Dispatch(ctx, dest)
			if err != nil {
				t.Fatal(err)
			}
			defer common.Interrupt(link.Reader)
			defer common.Close(link.Writer)
			reader, ok := link.Reader.(*pipe.Reader)
			if !ok {
				t.Fatalf("native mux/XUDP reader replaced: %T", link.Reader)
			}
			b := buf.FromBytes(payload)
			if network == "udp" {
				b.UDP = &dest
			}
			if err := link.Writer.WriteMultiBuffer(buf.MultiBuffer{b}); err != nil {
				t.Fatal(err)
			}
			mb, err := reader.ReadMultiBufferTimeout(3 * time.Second)
			if err != nil {
				t.Fatal(err)
			}
			if mb.String() != string(payload) {
				t.Fatalf("echo %q", mb.String())
			}
			buf.ReleaseMulti(mb)
			s := x.DrainAccess()
			if u, d := accessTotals(s); u != int64(len(payload)) || d != int64(len(payload)) {
				t.Fatalf("native directions %d/%d %+v", u, d, s)
			}
			if len(s.Entries) != 1 || s.Entries[0].IP != "127.0.0.1" || s.Entries[0].Port != int(dest.Port) || s.Entries[0].Network != network || s.Entries[0].DomainSource != "ip" || s.Entries[0].Connections != 1 {
				t.Fatalf("native metadata %+v", s)
			}
			raw, _, _, err := x.GetUserTraffic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if raw[123] != [2]int64{int64(len(payload)), int64(len(payload))} {
				t.Fatalf("native billing changed %+v", raw)
			}
		})
	}
}

type accessCaptureWriter struct {
	mu   sync.Mutex
	data bytes.Buffer
}

func (w *accessCaptureWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, b := range mb {
		w.data.Write(b.Bytes())
	}
	buf.ReleaseMulti(mb)
	return nil
}
func (w *accessCaptureWriter) Bytes() []byte {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]byte(nil), w.data.Bytes()...)
}

func TestAccessNativeDispatchLinkSniffAndOriginalStats(t *testing.T) {
	x := accessKernel(t)
	payload := []byte("GET / HTTP/1.1\r\nHost: video.example\r\n\r\n")
	dest := tcpAccessEcho(t, len(payload))
	ctx := session.ContextWithContent(context.WithValue(accessSession(), core.XrayKey(1), x.instance), &session.Content{SniffingRequest: session.SniffingRequest{Enabled: true, RouteOnly: true, OverrideDestinationForProtocol: []string{"http"}}})
	ctx = session.SetForcedOutboundTagToContext(ctx, "direct")
	capture := &accessCaptureWriter{}
	original := buf.NewReader(bytes.NewReader(payload))
	link := &transport.Link{Reader: original, Writer: capture}
	if err := x.limitDispatcher.DispatchLink(ctx, dest, link); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(capture.Bytes(), payload) {
		t.Fatalf("DispatchLink echo %q", capture.Bytes())
	}
	s := x.DrainAccess()
	var sniffed bool
	for _, e := range s.Entries {
		if e.Domain == "video.example" && e.DomainSource == "sniff" && e.IP == "127.0.0.1" && e.Download == int64(len(payload)) {
			sniffed = true
		}
	}
	if !sniffed {
		t.Fatalf("proven native sniff metadata missing %+v", s)
	}
	if u, d := accessTotals(s); u != int64(len(payload)) || d != int64(len(payload)) {
		t.Fatalf("sniff bytes %d/%d", u, d)
	}
	raw, _, _, err := x.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if raw[123] != [2]int64{int64(len(payload)), int64(len(payload))} {
		t.Fatalf("native stats %+v", raw)
	}
}

func TestAccessXrayPacketTargetsAndMuxOuterExcluded(t *testing.T) {
	ld := newTestDispatcher()
	c := access.NewCollector(access.Config{Enabled: true})
	ld.access.Store(c)
	ld.UpdateLimits(map[string]int{userEmail(123): 123}, nil, nil)
	request := net.UDPDestination(net.DomainAddress("request.example"), 53)
	_, f := ld.accessContext(accessSession(), request, userEmail(123))
	writer := &accessWriter{Writer: buf.Discard, flow: f, upload: true, target: destinationTarget(request)}
	ip := net.UDPDestination(net.IPAddress([]byte{192, 0, 2, 1}), 443)
	b1 := buf.FromBytes([]byte("abc"))
	b1.UDP = &request
	b2 := buf.FromBytes([]byte("12345"))
	b2.UDP = &ip
	writer.WriteMultiBuffer(buf.MultiBuffer{b1, b2})
	s := c.Drain()
	if len(s.Entries) != 2 {
		t.Fatalf("packet metadata %+v", s)
	}
	for _, e := range s.Entries {
		if e.Domain == "request.example" {
			if e.Upload != 3 || e.DomainSource != "request" {
				t.Fatalf("domain %+v", e)
			}
		} else if e.IP == "192.0.2.1" {
			if e.Upload != 5 || e.Port != 443 {
				t.Fatalf("IP %+v", e)
			}
		} else {
			t.Fatalf("unexpected %+v", e)
		}
	}
	_, outer := ld.accessContext(accessSession(), net.TCPDestination(net.DomainAddress("v1.mux.cool"), 0), userEmail(123))
	if outer != nil {
		t.Fatal("mux framing attributed as destination")
	}
}

func TestAccessPreservesNativeStatWriterSpliceCounterAndLimiter(t *testing.T) {
	c := access.NewCollector(access.Config{Enabled: true})
	target := access.Target{Domain: "splice.example", DomainSource: "request", Network: "tcp", Port: 443}
	flow := c.NewFlow(123, target)
	raw := &appstats.Counter{}
	native := &dispatcher.SizeStatWriter{Writer: buf.Discard, Counter: raw}
	wrapped := wrapAccessWriter(native, flow, target, false)
	if wrapped != native {
		t.Fatal("native SizeStatWriter identity lost")
	}
	wrapped.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("abc"))})
	// Native CopyRawConn calls this exact counter directly for splice bytes.
	native.Counter.Add(17)
	if raw.Value() != 20 {
		t.Fatalf("native billing counter %d", raw.Value())
	}
	if _, d := accessTotals(c.Drain()); d != 20 {
		t.Fatalf("direct splice counter %d", d)
	}
	if prior := native.Counter.Set(0); prior != 20 || native.Counter.Value() != 0 {
		t.Fatal("counter Set/Value changed")
	}
	if len(c.Drain().Entries) != 0 {
		t.Fatal("billing reset recorded access bytes")
	}
	limited := &dispatcher.RateLimitWriter{Writer: native, Limiter: rate.NewLimiter(1, 1), Context: context.Background()}
	if wrapAccessWriter(limited, flow, target, false) != limited {
		t.Fatal("native limiter replaced")
	}
	if err := limited.WriteMultiBuffer(buf.MultiBuffer{buf.FromBytes([]byte("exceeds-burst"))}); err == nil {
		t.Fatal("expected native limiter failure")
	}
	if raw.Value() != 0 || len(c.Drain().Entries) != 0 {
		t.Fatal("failed limiter created traffic")
	}
}

func TestAccessXrayKeepsCollectorAcrossRealReloadDisableAndRemoval(t *testing.T) {
	x := accessKernel(t)
	request := net.TCPDestination(net.DomainAddress("long.example"), 443)
	_, flow := x.limitDispatcher.accessContext(accessSession(), request, userEmail(123))
	flow.Add(3, 5)
	collector := x.accessCollector
	if u, d := accessTotals(x.DrainAccess()); u != 3 || d != 5 {
		t.Fatal("live slice missing")
	}
	users := []model.UserSpec{{ID: 123, UUID: "11111111-1111-1111-1111-111111111111"}}
	if err := x.Reload(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	if x.accessCollector != collector || x.limitDispatcher.access.Load() != collector {
		t.Fatal("reload replaced collector")
	}
	if _, err := x.RemoveUsers(users); err != nil {
		t.Fatal(err)
	}
	flow.Add(7, 11)
	x.ConfigureAccess(access.Config{})
	flow.Add(99, 99)
	if u, d := accessTotals(x.DrainAccess()); u != 7 || d != 11 {
		t.Fatalf("removed/disabled tail %d/%d", u, d)
	}
	if len(x.DrainAccess().Entries) != 0 {
		t.Fatal("disabled capture remains active")
	}
}
