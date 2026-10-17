package xray

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/model"
)

// Both ends use real TCP connections; only the selected loopback target is routed directly.
func newHTTPTrafficKernel(t *testing.T) (*Xray, *model.NodeSpec, *net.TCPListener, []model.UserSpec) {
	t.Helper()
	target, err := net.ListenTCP("tcp4", &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = target.Close() })
	disabled := false
	x := New(config.KernelConfig{
		Type: "xray", ConfigDir: t.TempDir(), LogLevel: "error",
		Egress: config.EgressConfig{EnableDefaultRules: &disabled},
		CustomRoute: []map[string]any{{
			"type": "field", "ip": []string{"127.0.0.1/32"},
			"port": fmt.Sprint(target.Addr().(*net.TCPAddr).Port), "outboundTag": "direct",
		}},
	})
	node := trafficTestNode(t)
	node.Protocol, node.ListenIP = "http", "127.0.0.1"
	users := []model.UserSpec{
		{ID: 101, UUID: "11111111-1111-4111-8111-111111111111"},
		{ID: 202, UUID: "22222222-2222-4222-8222-222222222222"},
	}
	if err := x.Start(node, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(x.Stop)
	return x, node, target, users
}

type httpTrafficTunnel struct {
	client net.Conn
	reader *bufio.Reader
	peer   net.Conn
}

func openHTTPTrafficTunnel(t *testing.T, node *model.NodeSpec, target *net.TCPListener, user model.UserSpec) *httpTrafficTunnel {
	t.Helper()
	client, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(node.ServerPort)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	if err := client.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	auth := base64.StdEncoding.EncodeToString([]byte(user.UUID + ":" + user.UUID))
	if _, err := fmt.Fprintf(client, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target.Addr(), target.Addr(), auth); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(client)
	response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT for user %d: %s", user.ID, response.Status)
	}
	if err := target.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	peer, err := target.AcceptTCP()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = peer.Close() })
	return &httpTrafficTunnel{client: client, reader: reader, peer: peer}
}

func (tunnel *httpTrafficTunnel) exchange(t *testing.T, up, down int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	if err := tunnel.client.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	if err := tunnel.peer.SetDeadline(deadline); err != nil {
		t.Fatal(err)
	}
	for _, transfer := range []struct {
		writer io.Writer
		reader io.Reader
		size   int
		fill   byte
	}{
		{tunnel.client, tunnel.peer, up, 0x35},
		{tunnel.peer, tunnel.reader, down, 0xa7},
	} {
		want := bytes.Repeat([]byte{transfer.fill}, transfer.size)
		if _, err := io.Copy(transfer.writer, bytes.NewReader(want)); err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(want))
		if _, err := io.ReadFull(transfer.reader, got); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("tunneled payload mismatch: size=%d", transfer.size)
		}
	}
}

func (tunnel *httpTrafficTunnel) close() {
	_ = tunnel.client.Close()
	_ = tunnel.peer.Close()
}

func requireHTTPTraffic(t *testing.T, x *Xray, want map[int][2]int64) {
	t.Helper()
	requireHTTPTrafficSample(t, x, want, false)
}

func requireHTTPActiveTraffic(t *testing.T, x *Xray, want map[int][2]int64) {
	t.Helper()
	// Native Linux HTTP splice publishes downlink bytes when ReadFrom ends.
	requireHTTPTrafficSample(t, x, want, runtime.GOOS == "linux" || runtime.GOOS == "android")
}

func requireHTTPTrafficSample(t *testing.T, x *Xray, want map[int][2]int64, deferDownload bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _, _, err := x.GetUserTraffic(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		matched := len(got) == len(want)
		for id, expected := range want {
			matched = matched && got[id][0] == expected[0] && (got[id][1] == expected[1] || (deferDownload && got[id][1] >= 0 && got[id][1] < expected[1]))
			if got[id][0] > expected[0] || got[id][1] > expected[1] {
				t.Fatalf("HTTP traffic=%v, want %v", got, want)
			}
		}
		if matched {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP traffic=%v, want %v", got, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitHTTPTrafficIdle(t *testing.T, x *Xray) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		x.mu.Lock()
		ld := x.limitDispatcher
		x.mu.Unlock()
		if ld == nil || ld.connCount.Load() == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("HTTP connections did not close: %d", ld.connCount.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestHTTPForwardRequestTrafficBilling(t *testing.T) {
	x, node, target, users := newHTTPTrafficKernel(t)
	body := "plain-http-result"
	reply := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", len(body), body)
	type observation struct {
		requestBytes int
		err          error
	}
	observed := make(chan observation, 1)
	_ = target.SetDeadline(time.Now().Add(5 * time.Second))
	go func() {
		peer, err := target.AcceptTCP()
		if err != nil {
			observed <- observation{err: err}
			return
		}
		defer peer.Close()
		_ = peer.SetDeadline(time.Now().Add(5 * time.Second))
		var wire bytes.Buffer
		request, err := http.ReadRequest(bufio.NewReader(io.TeeReader(peer, &wire)))
		if err == nil {
			_, err = io.Copy(io.Discard, request.Body)
			_ = request.Body.Close()
		}
		if err == nil {
			_, err = io.WriteString(peer, reply)
		}
		observed <- observation{requestBytes: wire.Len(), err: err}
	}()
	client, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(node.ServerPort)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	_ = client.SetDeadline(time.Now().Add(5 * time.Second))
	auth := base64.StdEncoding.EncodeToString([]byte(users[0].UUID + ":" + users[0].UUID))
	if _, err := fmt.Fprintf(client, "GET http://%s/profile HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\nConnection: close\r\n\r\n", target.Addr(), target.Addr(), auth); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(bufio.NewReader(client), &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK || string(got) != body {
		t.Fatalf("HTTP forward response: status=%d body=%q err=%v", response.StatusCode, got, err)
	}
	client.Close()
	result := <-observed
	if result.err != nil {
		t.Fatal(result.err)
	}
	// Compare billing with actual upstream wire bytes, including HTTP headers.
	requireHTTPTraffic(t, x, map[int][2]int64{users[0].ID: {int64(result.requestBytes), int64(len(reply))}})
}

func TestHTTPConnectTrafficBillingAndStopTail(t *testing.T) {
	x, node, target, users := newHTTPTrafficKernel(t)
	one := openHTTPTrafficTunnel(t, node, target, users[0])
	one.exchange(t, 1379, 2711)
	requireHTTPActiveTraffic(t, x, map[int][2]int64{101: {1379, 2711}})
	two := openHTTPTrafficTunnel(t, node, target, users[1])
	two.exchange(t, 4099, 853)
	want := map[int][2]int64{101: {1379, 2711}, 202: {4099, 853}}
	requireHTTPActiveTraffic(t, x, want)
	requireHTTPActiveTraffic(t, x, want)

	// Stop must retain bytes transferred after the final periodic sample.
	one.exchange(t, 101, 307)
	two.exchange(t, 43, 211)
	one.close()
	two.close()
	waitHTTPTrafficIdle(t, x)
	x.Stop()
	want = map[int][2]int64{101: {1480, 3018}, 202: {4142, 1064}}
	requireHTTPTraffic(t, x, want)
	requireHTTPTraffic(t, x, want)
}

func TestHTTPConnectTrafficBillingWithBandwidthLimit(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprintf("access=%v", enabled), func(t *testing.T) {
			x, node, target, users := newHTTPTrafficKernel(t)
			users[0].SpeedLimit = 8
			x.updateBandwidthLimits(users)
			x.ConfigureAccess(access.Config{Enabled: enabled})
			tunnel := openHTTPTrafficTunnel(t, node, target, users[0])
			tunnel.exchange(t, 1379, 2711)
			want := map[int][2]int64{users[0].ID: {1379, 2711}}
			// Limited streams must publish both directions while the tunnel is still open.
			requireHTTPTraffic(t, x, want)
			tunnel.exchange(t, 101, 307)
			tunnel.close()
			waitHTTPTrafficIdle(t, x)
			x.Stop()
			want[users[0].ID] = [2]int64{1480, 3018}
			requireHTTPTraffic(t, x, want)
			requireHTTPTraffic(t, x, want)
			up, down := accessTotals(x.DrainAccess())
			if enabled && (up != 1480 || down != 3018) {
				t.Fatalf("limited HTTP access traffic=(%d, %d), want (1480, 3018)", up, down)
			}
			if !enabled && (up != 0 || down != 0) {
				t.Fatalf("disabled HTTP access traffic=(%d, %d)", up, down)
			}
		})
	}
}

func TestHTTPConnectTrafficSurvivesUserReplacement(t *testing.T) {
	x, node, target, users := newHTTPTrafficKernel(t)
	one := openHTTPTrafficTunnel(t, node, target, users[0])
	two := openHTTPTrafficTunnel(t, node, target, users[1])
	one.exchange(t, 601, 907)
	two.exchange(t, 1201, 503)
	one.close()
	two.close()
	waitHTTPTrafficIdle(t, x)

	// HTTP has no UserManager: credential rotation replaces the real instance.
	oldInstance := x.instance
	users = append([]model.UserSpec(nil), users...)
	users[0].UUID = "33333333-3333-4333-8333-333333333333"
	// Windows cannot bind the old live inbound twice, so replacements use fresh ports.
	node.ServerPort = trafficTestNode(t).ServerPort
	added, removed, err := x.UpdateUsers(users)
	if err != nil {
		t.Fatal(err)
	}
	if added != 1 || removed != 0 || x.instance == oldInstance {
		t.Fatalf("credential rotation: added=%d removed=%d replaced=%v", added, removed, x.instance != oldInstance)
	}
	want := map[int][2]int64{101: {601, 907}, 202: {1201, 503}}
	requireHTTPTraffic(t, x, want)
	one = openHTTPTrafficTunnel(t, node, target, users[0])
	one.exchange(t, 313, 719)
	one.close()
	waitHTTPTrafficIdle(t, x)

	oldInstance = x.instance
	node.ServerPort = trafficTestNode(t).ServerPort
	removed, err = x.RemoveUsers(users[:1])
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 || x.instance == oldInstance {
		t.Fatalf("user removal: removed=%d replaced=%v", removed, x.instance != oldInstance)
	}
	two = openHTTPTrafficTunnel(t, node, target, users[1])
	two.exchange(t, 211, 433)
	two.close()
	waitHTTPTrafficIdle(t, x)
	want = map[int][2]int64{101: {914, 1626}, 202: {1412, 936}}
	requireHTTPTraffic(t, x, want)
	requireHTTPTraffic(t, x, want)
}
