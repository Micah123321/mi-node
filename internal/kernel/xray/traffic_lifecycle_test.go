package xray

import (
	"context"
	"net"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/tracker"
	xraystats "github.com/xtls/xray-core/features/stats"
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
func trafficCounter(t *testing.T, x *Xray, id int) xraystats.Counter {
	t.Helper()
	mgr := x.instance.GetFeature(xraystats.ManagerType()).(xraystats.Manager)
	name := counterName(id, "uplink")
	if c := mgr.GetCounter(name); c != nil {
		return c
	}
	return mustRegisterCounter(t, mgr, name)
}
func sampleTraffic(t *testing.T, x *Xray, tr *tracker.Tracker) {
	t.Helper()
	traffic, alive, count, err := x.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	tr.Process(traffic, alive, count)
}

func TestTrafficSurvivesRealXrayReloadAndStopStart(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray", ConfigDir: t.TempDir()})
	users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}}
	if err := x.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer x.Stop()
	tr := tracker.New()
	trafficCounter(t, x, 1).Add(100)
	sampleTraffic(t, x, tr)
	trafficCounter(t, x, 1).Add(25)
	if err := x.Reload(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	trafficCounter(t, x, 1).Add(200)
	sampleTraffic(t, x, tr)
	if got := tr.FlushTraffic()[1][0]; got != 325 {
		t.Fatalf("reload traffic=%d, want 325", got)
	}
	trafficCounter(t, x, 1).Add(30)
	x.Stop()
	sampleTraffic(t, x, tr)
	if err := x.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	trafficCounter(t, x, 1).Add(400)
	sampleTraffic(t, x, tr)
	if got := tr.FlushTraffic()[1][0]; got != 430 {
		t.Fatalf("stop/start traffic=%d, want 430", got)
	}
}

func TestTrafficSurvivesRealXrayUserRemoval(t *testing.T) {
	x := New(config.KernelConfig{Type: "xray", ConfigDir: t.TempDir()})
	users := []model.UserSpec{{ID: 1, UUID: "11111111-1111-1111-1111-111111111111"}, {ID: 2, UUID: "22222222-2222-2222-2222-222222222222"}}
	if err := x.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer x.Stop()
	tr := tracker.New()
	counter := trafficCounter(t, x, 1)
	counter.Add(100)
	sampleTraffic(t, x, tr)
	counter.Add(25)
	if _, err := x.RemoveUsers(users[:1]); err != nil {
		t.Fatal(err)
	}
	// A previously admitted connection can still write after its user is removed.
	counter.Add(50)
	sampleTraffic(t, x, tr)
	if got := tr.FlushTraffic()[1][0]; got != 175 {
		t.Fatalf("removed user traffic=%d, want 175", got)
	}
	trafficCounter(t, x, 2).Add(80)
	if _, err := x.RemoveUsers(users[1:]); err != nil {
		t.Fatal(err)
	}
	sampleTraffic(t, x, tr)
	if got := tr.FlushTraffic()[2][0]; got != 80 {
		t.Fatalf("last user tail=%d, want 80", got)
	}
}
