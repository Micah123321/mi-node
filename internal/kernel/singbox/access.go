package singbox

import (
	"github.com/micah123321/mi-node/internal/access"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/common/buf"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func (s *SingBox) ConfigureAccess(cfg access.Config) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.connTracker == nil {
		s.connTracker = NewConnTracker(0)
		s.connTracker.SetDisableDeviceGate(s.cfg.LowJitterDisableDeviceGate)
	}
	s.connTracker.access.Configure(cfg)
}

func (s *SingBox) DrainAccess() access.Snapshot {
	ct := s.connTrackerSafe()
	if ct == nil {
		return access.Snapshot{Entries: []access.Entry{}}
	}
	return ct.access.Drain()
}

func packetAccessTarget(dest singM.Socksaddr) access.Target {
	t := access.Target{Port: int(dest.Port), Network: "udp"}
	if dest.IsFqdn() {
		t.Domain = dest.Fqdn
		t.DomainSource = "request"
	}
	if dest.Addr.IsValid() {
		t.IP = dest.Addr.String()
	}
	return t
}

// sing UnwrapCountPacketWriter first follows Upstream, before consulting its
// counter interface. Keep the existing billing/limiter bypass behavior of that
// path, but expose a separate access counter with explicitly unknown target.
type packetAccessUpstream struct {
	N.PacketConn
	flow *access.Flow
}

func (c *packetAccessUpstream) WriterReplaceable() bool { return false }
func (c *packetAccessUpstream) ReaderReplaceable() bool { return true }
func (c *packetAccessUpstream) Upstream() any           { return c.PacketConn }
func (c *packetAccessUpstream) UnwrapPacketWriter() (N.PacketWriter, []N.CountFunc) {
	return c.PacketConn, []N.CountFunc{func(n int64) { c.flow.Add(0, n) }}
}
func (c *packetAccessUpstream) WritePacket(buffer *buf.Buffer, dest singM.Socksaddr) error {
	n := int64(buffer.Len())
	err := c.PacketConn.WritePacket(buffer, dest)
	if err == nil {
		c.flow.AddTo(packetAccessTarget(dest), 0, n)
	}
	return err
}

func accessTarget(m adapter.InboundContext, network string) access.Target {
	dest := m.Destination
	// RouteOriginalDestination retains the request before route overrides.
	if m.RouteOriginalDestination.IsValid() {
		dest = m.RouteOriginalDestination
	}
	// QUIC inbounds also put their listener address here; only fake-IP
	// translation makes OriginDestination a client-requested endpoint.
	if m.FakeIP && m.OriginDestination.IsValid() {
		dest = m.OriginDestination
	}
	t := packetAccessTarget(dest)
	t.Network = network
	// Resolution belongs to the current route host. Keep only an unambiguous
	// address, never an override's address or an arbitrary DNS candidate.
	if t.Domain != "" && t.IP == "" && dest.Fqdn == m.Destination.Fqdn && len(m.DestinationAddresses) == 1 {
		ip := m.DestinationAddresses[0].Unmap()
		if ip.IsValid() && !ip.IsUnspecified() {
			t.IP = ip.String()
		}
	}
	// Domain alone can come from the core's reverse DNS cache. These
	// successful sniffers explicitly overwrite it with Host/SNI metadata.
	if m.SniffError == nil && m.Domain != "" && (m.Protocol == "http" || m.Protocol == "tls" || m.Protocol == "quic") {
		if t.Domain != "" && t.Domain != m.Domain {
			return t
		}
		t.Domain = m.Domain
		t.DomainSource = "sniff"
	}
	return t
}
