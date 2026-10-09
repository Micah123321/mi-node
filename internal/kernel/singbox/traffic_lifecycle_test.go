package singbox

import (
	"context"
	"net"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/tracker"
)

func trafficTestNode(t *testing.T) *model.NodeSpec {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	return &model.NodeSpec{Protocol: "vless", ServerPort: port}
}
func sampleTraffic(t *testing.T, s *SingBox, tr *tracker.Tracker) {
	t.Helper()
	traffic, alive, count, err := s.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr.Process(traffic, alive, count)
}
func TestTrafficSurvivesRealSingBoxRestartAndDrainingConnections(t *testing.T) {
	s := New(config.KernelConfig{Type: "sing-box", ConfigDir: t.TempDir()})
	users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}}
	if err := s.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	tr := tracker.New()
	old := s.connTracker
	conn := old.RoutedConnection(context.Background(), &testConn{}, testInboundContext(users[0].UUID, "127.0.0.1"), nil, nil)
	conn.Write(make([]byte, 100))
	sampleTraffic(t, s, tr)
	if err := s.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	// These bytes arrive through a connection belonging to the old instance.
	conn.Write(make([]byte, 25))
	conn.Close()
	s.recycleWG.Wait()
	s.connTracker.users[1].download.Add(200)
	sampleTraffic(t, s, tr)
	if got := tr.FlushTraffic()[1][1]; got != 325 {
		t.Fatalf("restart traffic=%d, want 325", got)
	}
	s.connTracker.users[1].download.Add(30)
	s.Stop()
	sampleTraffic(t, s, tr)
	if err := s.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	s.connTracker.users[1].download.Add(400)
	sampleTraffic(t, s, tr)
	if got := tr.FlushTraffic()[1][1]; got != 430 {
		t.Fatalf("stop/start traffic=%d, want 430", got)
	}
}

func TestTrafficSurvivesRealSingBoxReloadAndUserRemoval(t *testing.T) {
	s := New(config.KernelConfig{Type: "sing-box", ConfigDir: t.TempDir()})
	users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}, {ID: 2, UUID: "22222222-2222-2222-2222-222222222222"}}
	nc := trafficTestNode(t)
	if err := s.Start(nc, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	tr := tracker.New()
	conn := s.connTracker.RoutedConnection(context.Background(), &testConn{}, testInboundContext(users[0].UUID, "127.0.0.1"), nil, nil)
	conn.Write(make([]byte, 100))
	sampleTraffic(t, s, tr)
	if err := s.Reload(nc, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RemoveUsers(users[:1]); err != nil {
		t.Fatal(err)
	}
	conn.Write(make([]byte, 75))
	conn.Close()
	sampleTraffic(t, s, tr)
	if got := tr.FlushTraffic()[1][1]; got != 175 {
		t.Fatalf("deleted user tail=%d, want 175", got)
	}
}
