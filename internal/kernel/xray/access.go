package xray

import (
	"context"

	"github.com/micah123321/mi-node/internal/access"
	dispatcher "github.com/xtls/xray-core/app/dispatcher"
	"github.com/xtls/xray-core/common"
	"github.com/xtls/xray-core/common/buf"
	"github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/session"
	"github.com/xtls/xray-core/core"
	"github.com/xtls/xray-core/features/bandwidth"
	"github.com/xtls/xray-core/features/dns"
	"github.com/xtls/xray-core/features/outbound"
	"github.com/xtls/xray-core/features/policy"
	"github.com/xtls/xray-core/features/routing"
	"github.com/xtls/xray-core/features/stats"
	"github.com/xtls/xray-core/transport"
)

func (x *Xray) ConfigureAccess(cfg access.Config) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.accessCollector == nil {
		x.accessCollector = access.NewCollector(access.Config{})
	}
	x.accessCollector.Configure(cfg)
	if x.limitDispatcher != nil {
		x.limitDispatcher.access.Store(x.accessCollector)
	}
}

func (x *Xray) DrainAccess() access.Snapshot {
	x.mu.Lock()
	collector := x.accessCollector
	x.mu.Unlock()
	return collector.Drain()
}

type accessFlowKey struct{}

func (d *LimitDispatcher) accessContext(ctx context.Context, dest net.Destination, email string) (context.Context, *access.Flow) {
	collector := d.access.Load()
	// Mux outer frames are transport overhead, not an attributed destination.
	if collector == nil || dest.Address == nil || (dest.Address.Family().IsDomain() && dest.Address.Domain() == "v1.mux.cool") {
		return ctx, nil
	}
	d.mu.RLock()
	uid := d.emailToUID[email]
	d.mu.RUnlock()
	if uid <= 0 {
		return ctx, nil
	}
	flow := collector.NewFlow(uid, destinationTarget(dest))
	return context.WithValue(ctx, accessFlowKey{}, flow), flow
}

func destinationTarget(dest net.Destination) access.Target {
	t := access.Target{Port: int(dest.Port)}
	switch dest.Network {
	case net.Network_TCP:
		t.Network = "tcp"
	case net.Network_UDP:
		t.Network = "udp"
	}
	if dest.Address != nil {
		if dest.Address.Family().IsDomain() {
			t.Domain = dest.Address.Domain()
			t.DomainSource = "request"
		} else {
			t.IP = dest.Address.IP().String()
		}
	}
	return t
}

// Read metadata exactly once at the routed handler boundary, after the native
// sniffer finishes. Byte callbacks do not race with mutable session metadata.
func routedAccessTarget(ctx context.Context) access.Target {
	obs := session.OutboundsFromContext(ctx)
	if len(obs) == 0 {
		return access.Target{}
	}
	ob := obs[len(obs)-1]
	t := destinationTarget(ob.OriginalTarget)
	if t.Domain != "" {
		return t
	}
	content := session.ContentFromContext(ctx)
	if content == nil || (content.Protocol != "http1" && content.Protocol != "http2" && content.Protocol != "tls" && content.Protocol != "quic") {
		return t
	}
	sniffed := ob.Target
	if ob.RouteTarget.Address != nil && ob.RouteTarget.Address.Family().IsDomain() {
		sniffed = ob.RouteTarget
	}
	if sniffed.Address != nil && sniffed.Address.Family().IsDomain() {
		t.Domain = sniffed.Address.Domain()
		t.DomainSource = "sniff"
	}
	return t
}

// The native dispatcher resolves these same features before Init. Registering
// this second callback preserves its policy/stats/bandwidth and FakeDNS setup,
// while hooking the writer at the real outbound handoff. No core files change.
func installAccessOutboundHook(ctx context.Context, config interface{}, inner interface{}) error {
	native, ok := inner.(*dispatcher.DefaultDispatcher)
	if !ok {
		return nil
	}
	return core.RequireFeatures(ctx, func(om outbound.Manager, router routing.Router, pm policy.Manager, sm stats.Manager, bm bandwidth.Manager, _ dns.Client) error {
		return native.Init(config.(*dispatcher.Config), &accessOutboundManager{Manager: om}, router, pm, sm, bm)
	})
}

type accessOutboundManager struct{ outbound.Manager }

func (m *accessOutboundManager) GetHandler(tag string) outbound.Handler {
	return accessHandler(m.Manager.GetHandler(tag))
}
func (m *accessOutboundManager) GetDefaultHandler() outbound.Handler {
	return accessHandler(m.Manager.GetDefaultHandler())
}
func accessHandler(h outbound.Handler) outbound.Handler {
	if h == nil {
		return nil
	}
	return &accessOutboundHandler{Handler: h}
}

type accessOutboundHandler struct{ outbound.Handler }

func (h *accessOutboundHandler) Dispatch(ctx context.Context, link *transport.Link) {
	if flow, ok := ctx.Value(accessFlowKey{}).(*access.Flow); ok {
		target := routedAccessTarget(ctx)
		flow.SetTarget(target)
		link.Writer = wrapAccessWriter(link.Writer, flow, target, false)
	}
	h.Handler.Dispatch(ctx, link)
}

// Preserve native SizeStatWriter identity: Linux splice type-asserts this exact
// writer and calls Counter.Add directly. Decoration also leaves rate limiting
// outside the access hook, so canceled waits do not invent counted bytes.
func wrapAccessWriter(writer buf.Writer, flow *access.Flow, target access.Target, upload bool) buf.Writer {
	switch w := writer.(type) {
	case *dispatcher.RateLimitWriter:
		w.Writer = wrapAccessWriter(w.Writer, flow, target, upload)
		return w
	case *dispatcher.SizeStatWriter:
		if target.Network == "tcp" {
			w.Counter = &accessStatsCounter{Counter: w.Counter, flow: flow, upload: upload}
		} else {
			w.Writer = &accessWriter{Writer: w.Writer, flow: flow, target: target, upload: upload}
		}
		return w
	default:
		return &accessWriter{Writer: writer, flow: flow, target: target, upload: upload}
	}
}

type accessStatsCounter struct {
	stats.Counter
	flow   *access.Flow
	upload bool
}

func (c *accessStatsCounter) Add(n int64) int64 {
	previous := c.Counter.Add(n)
	if c.upload {
		c.flow.Add(n, 0)
	} else {
		c.flow.Add(0, n)
	}
	return previous
}

// Like native SizeStatWriter, count at the writer boundary before the buffer is
// handed off and released. Billing still uses only the native stats counters.
type accessWriter struct {
	buf.Writer
	flow   *access.Flow
	upload bool
	target access.Target
}

func (w *accessWriter) WriteMultiBuffer(mb buf.MultiBuffer) error {
	countAccessBuffers(w.flow, w.target, w.upload, mb)
	return w.Writer.WriteMultiBuffer(mb)
}
func (w *accessWriter) Close() error { return common.Close(w.Writer) }
func (w *accessWriter) Interrupt()   { common.Interrupt(w.Writer) }

// Only DispatchLink's externally supplied reader uses this wrapper. The core
// keeps its own TimeoutWrapperReader/cachedReader and their timeout semantics.
type accessReader struct {
	buf.Reader
	flow   *access.Flow
	target access.Target
}

func (r *accessReader) ReadMultiBuffer() (buf.MultiBuffer, error) {
	mb, err := r.Reader.ReadMultiBuffer()
	countAccessBuffers(r.flow, r.target, true, mb)
	return mb, err
}
func (r *accessReader) Close() error { return common.Close(r.Reader) }
func (r *accessReader) Interrupt()   { common.Interrupt(r.Reader) }

func countAccessBuffers(flow *access.Flow, target access.Target, upload bool, mb buf.MultiBuffer) {
	for _, b := range mb {
		if b == nil {
			continue
		}
		n := int64(b.Len())
		var u, d int64
		if upload {
			u = n
		} else {
			d = n
		}
		if target.Network == "udp" {
			packetTarget := target
			if b.UDP != nil {
				packetTarget = destinationTarget(*b.UDP)
			}
			flow.AddTo(packetTarget, u, d)
		} else {
			flow.Add(u, d)
		}
	}
}
