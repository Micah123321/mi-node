//go:build with_quic

package singbox

import (
	"context"
	"net"
	"testing"

	"github.com/micah123321/mi-node/internal/cert"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/sagernet/sing-box/adapter"
	"github.com/sagernet/sing/service"
)

func TestSingBoxStart_TUICWithShadowsocksEgress_UsesDefaultShadowsocks(t *testing.T) {
	tmpDir := t.TempDir()

	certManager := cert.NewManager(config.CertConfig{
		CertMode: "self",
		CertDir:  tmpDir,
		Domain:   "node.example.com",
	})
	if err := certManager.Start(context.Background()); err != nil {
		t.Fatalf("start self-signed cert manager: %v", err)
	}

	packetConn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}
	port := packetConn.LocalAddr().(*net.UDPAddr).Port
	_ = packetConn.Close()

	kcfg := config.KernelConfig{
		ConfigDir: tmpDir,
		LogLevel:  "warn",
	}
	kcfg.Egress.Shadowsocks.Address = "127.0.0.1"
	kcfg.Egress.Shadowsocks.Port = 8388
	kcfg.Egress.Shadowsocks.Method = "aes-128-gcm"
	kcfg.Egress.Shadowsocks.Password = "test-password"

	nc := &model.NodeSpec{
		Protocol:          "tuic",
		ServerPort:        port,
		ServerName:        "node.example.com",
		CongestionControl: "bbr",
	}

	sb := New(kcfg)
	if err := sb.Start(nc, testUsers[:1], certManager.TLSCert()); err != nil {
		t.Fatalf("start sing-box: %v", err)
	}
	defer sb.Stop()

	outboundManager := service.FromContext[adapter.OutboundManager](sb.ctx)
	if outboundManager == nil {
		t.Fatal("expected outbound manager in sing-box context")
	}

	defaultOutbound := outboundManager.Default()
	if defaultOutbound == nil {
		t.Fatal("expected default outbound to be initialized")
	}
	if got := defaultOutbound.Tag(); got != config.DefaultShadowsocksProxyTag {
		t.Fatalf("default outbound tag: got %q, want %q", got, config.DefaultShadowsocksProxyTag)
	}
}
