package singbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/cert"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/sagernet/sing-box/adapter"
	singLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/anytls"
	"github.com/sagernet/sing/common"
	singM "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

func anyTLSExchange(s *SingBox, node *model.NodeSpec, user model.UserSpec, destination net.Addr) error {
	client, err := anytls.NewOutbound(s.ctx, service.FromContext[adapter.Router](s.ctx),
		singLog.NewNOPFactory().NewLogger("anytls-test"), "anytls-test", option.AnyTLSOutboundOptions{
			ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: uint16(node.ServerPort)},
			Password:      user.UUID,
			OutboundTLSOptionsContainer: option.OutboundTLSOptionsContainer{TLS: &option.OutboundTLSOptions{
				Enabled: true, ServerName: "node.example.invalid", Certificate: []string{string(s.tls.CertPEM)},
			}},
		})
	if err != nil {
		return err
	}
	defer common.Close(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := client.DialContext(ctx, "tcp", singM.SocksaddrFromNet(destination))
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	payload := []byte("anytls-protocol-switch-echo")
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	reply := make([]byte, len(payload))
	if _, err := io.ReadFull(conn, reply); err != nil {
		return err
	}
	if !bytes.Equal(reply, payload) {
		return fmt.Errorf("echo mismatch: %q", reply)
	}
	return nil
}

func TestProtocolReloadAnyTLSToMieru(t *testing.T) {
	for _, transport := range []string{"TCP", "UDP"} {
		for _, changePort := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/change_port=%v", transport, changePort), func(t *testing.T) {
				certManager := cert.NewManager(config.CertConfig{CertMode: "self", CertDir: t.TempDir(), Domain: "node.example.invalid"})
				if err := certManager.Start(context.Background()); err != nil {
					t.Fatal(err)
				}
				disabled := false
				s := New(config.KernelConfig{Type: "sing-box", ConfigDir: t.TempDir(), LogLevel: "error",
					Egress:      config.EgressConfig{EnableDefaultRules: &disabled, PreferIPv4: &disabled},
					CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.0/8"}, "outbound": "direct"}},
				})
				node := trafficTestNode(t)
				node.Protocol, node.ServerName = "anytls", "node.example.invalid"
				users := childTestUsers(t)[:1]
				echo := mieruEchoServer(t)
				if err := s.Start(node, users, certManager.TLSCert()); err != nil {
					t.Fatal(err)
				}
				defer s.Stop()
				if err := anyTLSExchange(s, node, users[0], echo); err != nil {
					t.Fatalf("initial AnyTLS: %v", err)
				}
				originalBox, originalTracker := s.box, s.connTracker
				next := *node
				next.Protocol, next.Transport = "mieru", transport
				if changePort {
					next.ServerPort = trafficTestNode(t).ServerPort
				}
				invalid := next
				invalid.Transport = "invalid"
				if err := s.Reload(&invalid, users, certManager.TLSCert()); err == nil {
					t.Fatal("invalid replacement unexpectedly succeeded")
				}
				if s.nodeConfig != node {
					t.Fatal("failed replacement committed the target config")
				}
				if err := anyTLSExchange(s, node, users[0], echo); err != nil {
					t.Fatalf("AnyTLS was not restored after invalid replacement: %v", err)
				}
				if changePort {
					var blocker io.Closer
					var err error
					address := fmt.Sprintf(":%d", next.ServerPort)
					if transport == "UDP" {
						blocker, err = net.ListenPacket("udp", address)
					} else {
						blocker, err = net.Listen("tcp", address)
					}
					if err != nil {
						t.Fatal(err)
					}
					defer blocker.Close()
					if err := s.Reload(&next, users, certManager.TLSCert()); err == nil {
						t.Fatal("busy Mieru port unexpectedly succeeded")
					}
					if err := anyTLSExchange(s, node, users[0], echo); err != nil {
						t.Fatalf("AnyTLS was not restored after busy target port: %v", err)
					}
					_ = blocker.Close()
				}
				if err := s.Reload(&next, users, certManager.TLSCert()); err != nil {
					t.Fatalf("AnyTLS -> Mieru: %v", err)
				}
				manager := service.FromContext[adapter.InboundManager](s.ctx)
				if _, exists := manager.Get("anytls-in"); exists {
					t.Fatal("obsolete AnyTLS inbound still registered")
				}
				if len(manager.Inbounds()) != 1 {
					t.Fatalf("unexpected listeners: %d", len(manager.Inbounds()))
				}
				if transport == "UDP" || changePort {
					listener, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", node.ServerPort))
					if err != nil {
						t.Fatalf("old AnyTLS TCP port not released: %v", err)
					}
					_ = listener.Close()
				}
				if err := mieruExchange(s, &next, users[0], echo); err != nil {
					t.Fatalf("Mieru after protocol switch: %v", err)
				}
				if transport == "UDP" {
					tcp := next
					tcp.Transport = "TCP"
					if err := s.Reload(&tcp, users, certManager.TLSCert()); err != nil {
						t.Fatal(err)
					}
					if err := mieruExchange(s, &tcp, users[0], echo); err != nil {
						t.Fatalf("Mieru UDP -> TCP: %v", err)
					}
					listener, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", next.ServerPort))
					if err != nil {
						t.Fatalf("previous Mieru UDP port retained after TCP switch: %v", err)
					}
					_ = listener.Close()
					if err := s.Reload(&next, users, certManager.TLSCert()); err != nil {
						t.Fatal(err)
					}
					if err := mieruExchange(s, &next, users[0], echo); err != nil {
						t.Fatalf("Mieru TCP -> UDP: %v", err)
					}
					previousPort := next.ServerPort
					moved := next
					moved.ServerPort = trafficTestNode(t).ServerPort
					if err := s.Reload(&moved, users, certManager.TLSCert()); err != nil {
						t.Fatal(err)
					}
					next = moved
					listener, err = net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", previousPort))
					if err != nil {
						t.Fatalf("previous Mieru UDP port retained after port change: %v", err)
					}
					_ = listener.Close()
					if err := mieruExchange(s, &next, users[0], echo); err != nil {
						t.Fatalf("Mieru UDP port change: %v", err)
					}
				}
				if err := s.Reload(node, users, certManager.TLSCert()); err != nil {
					t.Fatalf("Mieru -> AnyTLS: %v", err)
				}
				if _, exists := manager.Get("mieru-in"); exists {
					t.Fatal("obsolete Mieru inbound still registered")
				}
				if len(manager.Inbounds()) != 1 {
					t.Fatalf("unexpected listeners after return: %d", len(manager.Inbounds()))
				}
				if transport == "UDP" {
					listener, err := net.ListenPacket("udp", fmt.Sprintf("127.0.0.1:%d", next.ServerPort))
					if err != nil {
						t.Fatalf("old Mieru UDP port not released: %v", err)
					}
					_ = listener.Close()
				}
				if err := anyTLSExchange(s, node, users[0], echo); err != nil {
					t.Fatalf("AnyTLS after return: %v", err)
				}
				if s.box != originalBox || s.connTracker != originalTracker {
					t.Fatal("protocol reload replaced the box or traffic tracker")
				}
				traffic, _, _, err := s.GetUserTraffic(context.Background())
				if err != nil || traffic[users[0].ID][0] == 0 || traffic[users[0].ID][1] == 0 {
					t.Fatalf("lost traffic across protocol switch: %v, %v", traffic, err)
				}
			})
		}
	}
}
