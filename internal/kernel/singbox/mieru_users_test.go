package singbox

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	mieruLog "github.com/enfein/mieru/v3/pkg/log"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/sagernet/sing-box/adapter"
	singLog "github.com/sagernet/sing-box/log"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-box/protocol/mieru"
	"github.com/sagernet/sing/common"
	singM "github.com/sagernet/sing/common/metadata"
	"github.com/sagernet/sing/service"
)

func mieruEchoServer(t *testing.T) net.Addr {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr()
}

func mieruExchange(s *SingBox, node *model.NodeSpec, user model.UserSpec, destination net.Addr) error {
	client, err := mieru.NewOutbound(s.ctx, service.FromContext[adapter.Router](s.ctx),
		singLog.NewNOPFactory().NewLogger("mieru-test"), "mieru-test", option.MieruOutboundOptions{
			ServerOptions: option.ServerOptions{Server: "127.0.0.1", ServerPort: uint16(node.ServerPort)},
			Transport:     node.Transport, UserName: user.UUID, Password: user.UUID,
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
	payload := []byte("mieru-child-identity-echo")
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

func TestMieruUsersFollowChildIdentityUpdates(t *testing.T) {
	mieruLog.SetCallback(func(msg mieruLog.LogMessage) {
		if strings.Contains(msg.Message, "ListenPacket() failed") {
			t.Errorf("Mieru UDP listener error: %s", msg.Message)
		}
	})
	t.Cleanup(func() { mieruLog.SetCallback(nil) })
	for _, transport := range []string{"TCP", "UDP"} {
		t.Run(transport, func(t *testing.T) {
			users := childTestUsers(t)
			echo := mieruEchoServer(t)
			node := trafficTestNode(t)
			node.Protocol, node.Transport = "mieru", transport
			if transport == "UDP" {
				listener, err := net.ListenPacket("udp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				node.ServerPort = listener.LocalAddr().(*net.UDPAddr).Port
				_ = listener.Close()
			}
			disabled := false
			s := New(config.KernelConfig{
				Type: "sing-box", ConfigDir: t.TempDir(), LogLevel: "error",
				Egress: config.EgressConfig{EnableDefaultRules: &disabled, PreferIPv4: &disabled},
				// Only this fixture's loopback echo bypasses the private-address block.
				CustomRoute: []map[string]any{{"ip_cidr": []string{"127.0.0.0/8"}, "outbound": "direct"}},
			})
			if err := s.Start(node, users[:1], kernel.TLSCert{}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Stop)
			assertAccepted := func(user model.UserSpec) {
				t.Helper()
				if err := mieruExchange(s, node, user, echo); err != nil {
					t.Fatalf("authentication ID %d should connect: %v", user.ID, err)
				}
			}
			assertRejected := func(user model.UserSpec) {
				t.Helper()
				if err := mieruExchange(s, node, user, echo); err == nil {
					t.Fatalf("removed authentication ID %d still connects", user.ID)
				}
			}
			assertAccepted(users[0])

			if added, err := s.AddUsers(users[1:2]); err != nil || added != 1 {
				t.Fatalf("add child identity: added=%d error=%v", added, err)
			}
			assertAccepted(users[1])
			assertAccepted(users[0])

			if removed, err := s.RemoveUsers(users[1:2]); err != nil || removed != 1 {
				t.Fatalf("remove child identity: removed=%d error=%v", removed, err)
			}
			assertRejected(users[1])
			assertAccepted(users[0])

			if added, removed, err := s.UpdateUsers(users); err != nil || added != 2 || removed != 0 {
				t.Fatalf("full child sync: added=%d removed=%d error=%v", added, removed, err)
			}
			assertAccepted(users[1])
			assertAccepted(users[2])

			manager := service.FromContext[adapter.InboundManager](s.ctx)
			before, _ := manager.Get("mieru-in")
			if _, _, err := s.UpdateUsers(users); err != nil {
				t.Fatal(err)
			}
			after, _ := manager.Get("mieru-in")
			if before != after {
				t.Fatal("unchanged user sync recreated the listener")
			}

			rotated := users[1]
			rotated.UUID = "44444444-4444-4444-8444-444444444444"
			if _, err := s.AddUsers([]model.UserSpec{rotated}); err != nil {
				t.Fatal(err)
			}
			assertRejected(users[1])
			assertAccepted(rotated)

			traffic, _, _, err := s.GetUserTraffic(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			for _, user := range users {
				if traffic[user.ID][0] == 0 || traffic[user.ID][1] == 0 {
					t.Fatalf("lost per-identity traffic for %d: %v", user.ID, traffic)
				}
			}
		})
	}
}
