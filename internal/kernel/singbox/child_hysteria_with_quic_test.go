//go:build with_quic

package singbox

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/cert"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/limiter"
	"github.com/micah123321/mi-node/internal/model"
	box "github.com/sagernet/sing-box"
	"github.com/sagernet/sing-box/adapter"
	boxTLS "github.com/sagernet/sing-box/common/tls"
	"github.com/sagernet/sing-box/include"
	"github.com/sagernet/sing-box/option"
	"github.com/sagernet/sing-quic/hysteria2"
	singJSON "github.com/sagernet/sing/common/json"
	"github.com/sagernet/sing/common/logger"
	singM "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
	"github.com/sagernet/sing/service"
)

func TestChildIdentityHysteria2SharedQUICListener(t *testing.T) {
	users := childTestUsers(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	tmpDir := t.TempDir()
	certManager := cert.NewManager(config.CertConfig{
		CertMode: "self", CertDir: tmpDir, Domain: "node.example.com",
	})
	if err := certManager.Start(ctx); err != nil {
		t.Fatalf("generate self-signed certificate: %v", err)
	}
	t.Cleanup(certManager.Stop)

	echo, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var echoWG sync.WaitGroup
	echoWG.Add(1)
	go func() {
		defer echoWG.Done()
		for {
			conn, err := echo.Accept()
			if err != nil {
				return
			}
			echoWG.Add(1)
			go func() {
				defer echoWG.Done()
				defer conn.Close()
				deadline, _ := ctx.Deadline()
				if err := conn.SetDeadline(deadline); err != nil {
					return
				}
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		_ = echo.Close()
		echoWG.Wait()
	})

	cfg := buildConfig(config.KernelConfig{ConfigDir: tmpDir, LogLevel: "info"},
		&model.NodeSpec{Protocol: "hysteria", Version: 2, ServerName: "node.example.com"},
		users, certManager.TLSCert())
	// Keep the production builder and native inbound, with a loopback-only port 0.
	cfg["inbounds"].([]M)[0]["listen"] = "127.0.0.1"
	// The echo fixture must bypass the production route blocking private addresses.
	cfg["route"] = M{"final": "direct"}
	logPath := filepath.Join(tmpDir, "hysteria.log")
	cfg["log"].(M)["output"] = logPath
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	boxCtx := include.Context(ctx)
	opts, err := singJSON.UnmarshalExtendedContext[option.Options](boxCtx, data)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := box.New(box.Options{Context: boxCtx, Options: opts})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = instance.Close() })
	ct := NewConnTracker(0)
	ct.SetUserMap(buildUserMap(users), buildAccountMap(users))
	limits := limiter.New()
	limits.UpdateUsers(users)
	speed := limiter.NewSpeedTracker(limits)
	speed.UpdateBuckets()
	ct.SetSpeedLimitFunc(speed.GetLimiter)
	ct.SetDeviceLimitFunc(limits.GetDeviceLimitByUUID)
	ct.access.Configure(access.Config{Enabled: true})
	router := service.FromContext[adapter.Router](boxCtx)
	if router == nil {
		t.Fatal("native router missing")
	}
	router.AppendTracker(ct)
	if err := instance.Start(); err != nil {
		t.Fatalf("start Hysteria2 inbound: %v", err)
	}
	// The native inbound exposes its ephemeral port through its startup log.
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	var serverAddress singM.Socksaddr
	for _, line := range strings.Split(string(logData), "\n") {
		_, address, ok := strings.Cut(line, "udp server started at ")
		if !ok {
			continue
		}
		addr, err := netip.ParseAddrPort(strings.TrimSpace(address))
		if err != nil || addr.Addr().String() != "127.0.0.1" || addr.Port() == 0 {
			t.Fatalf("unexpected UDP listener %q: %v", address, err)
		}
		serverAddress = singM.ParseSocksaddr(addr.String())
	}
	if serverAddress.Port == 0 {
		t.Fatal("Hysteria2 listener address missing from startup log")
	}

	want := make(map[int][2]int64, len(users))
	for i, user := range users {
		tlsConfig, err := boxTLS.NewClient(ctx, logger.NOP(), "127.0.0.1", option.OutboundTLSOptions{
			Enabled: true, ServerName: "node.example.com",
			Certificate: []string{string(certManager.TLSCert().CertPEM)},
		})
		if err != nil {
			t.Fatal(err)
		}
		client, err := hysteria2.NewClient(hysteria2.ClientOptions{
			Context: ctx, Logger: logger.NOP(), TLSConfig: tlsConfig,
			Dialer: &N.DefaultDialer{Dialer: net.Dialer{
				Timeout: 5 * time.Second, LocalAddr: &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)},
			}},
			ServerAddress: serverAddress, Password: user.UUID,
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = client.CloseWithError(nil) })
		conn, err := client.DialConn(ctx, singM.SocksaddrFromNet(echo.Addr()))
		if err != nil {
			t.Fatalf("auth ID %d dial: %v", user.ID, err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		deadline, _ := ctx.Deadline()
		if err := conn.SetDeadline(deadline); err != nil {
			t.Fatal(err)
		}
		payload := bytes.Repeat([]byte{byte('a' + i)}, (i+1)*257)
		if n, err := conn.Write(payload); err != nil || n != len(payload) {
			t.Fatalf("auth ID %d write: n=%d err=%v", user.ID, n, err)
		}
		reply := make([]byte, len(payload))
		if _, err := io.ReadFull(conn, reply); err != nil {
			t.Fatalf("auth ID %d echo: %v", user.ID, err)
		}
		if !bytes.Equal(reply, payload) {
			t.Fatalf("auth ID %d echo mismatch", user.ID)
		}
		want[user.ID] = [2]int64{int64(len(payload)), int64(len(payload))}
	}

	// QUIC delivery can precede the server's counter callback; wait for both views.
	entries := make(map[int]access.Entry, len(users))
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot := ct.access.Drain()
		if snapshot.DroppedEntries != 0 {
			t.Fatalf("access entries dropped: %d", snapshot.DroppedEntries)
		}
		for _, entry := range snapshot.Entries {
			if _, ok := want[entry.UserID]; !ok {
				t.Fatalf("access lost auth ID: %+v", entry)
			}
			if entry.IP != "127.0.0.1" || entry.Port != echo.Addr().(*net.TCPAddr).Port || entry.Network != "tcp" {
				t.Errorf("access destination: got %s:%d/%s, want %s/tcp (auth ID %d)",
					entry.IP, entry.Port, entry.Network, echo.Addr(), entry.UserID)
			}
			total := entries[entry.UserID]
			total.Upload += entry.Upload
			total.Download += entry.Download
			total.Connections += entry.Connections
			entries[entry.UserID] = total
		}
		traffic, alive, count := ct.GetUserTraffic()
		if len(alive) != 1 || len(alive[7]) != 1 || !alive[7]["127.0.0.1"] || count != len(users) {
			t.Fatalf("alive must contain only real account 7: alive=%v connections=%d", alive, count)
		}
		complete := len(traffic) == len(want) && len(entries) == len(want)
		for id, expected := range want {
			e := entries[id]
			if traffic[id] != expected || e.Upload != expected[0] || e.Download != expected[1] || e.Connections != 1 {
				complete = false
			}
		}
		if complete {
			t.Logf("verified three TCP echoes and per-auth traffic/access: traffic=%v alive=%v connections=%d", traffic, alive, count)
			break
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatalf("per-auth accounting: traffic=%v access=%v want=%v: %v", traffic, entries, want, ctx.Err())
		}
	}
}
