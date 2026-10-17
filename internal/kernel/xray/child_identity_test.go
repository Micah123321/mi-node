package xray

import (
	"context"
	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/limiter"
	"github.com/micah123321/mi-node/internal/model"
	xnet "github.com/xtls/xray-core/common/net"
	featurebandwidth "github.com/xtls/xray-core/features/bandwidth"
	"strconv"
	"testing"
)

func childTestUsers(t *testing.T) []model.UserSpec {
	t.Helper()
	if strconv.IntSize != 64 {
		t.Skip("child identities require 64-bit ints")
	}
	base := int64(1) << 40
	return []model.UserSpec{{ID: 7, UUID: "parent", DeviceLimit: 1, SpeedLimit: 8}, {ID: int(base + 1), UUID: "one", BillingUserID: 7, BillingNodeID: 11, DeviceLimit: 1, SpeedLimit: 8}, {ID: int(base + 2), UUID: "two", BillingUserID: 7, BillingNodeID: 12, DeviceLimit: 1, SpeedLimit: 8}}
}

func TestChildIdentityXrayIndependentCountersSharedAdmissionAndTail(t *testing.T) {
	users := childTestUsers(t)
	d := newTestDispatcher()
	configureDispatcherLimits(d, users)
	collector := access.NewCollector(access.Config{Enabled: true})
	d.access.Store(collector)
	x, mgr := newStatsBackedXray(t, users, d)
	links := make([]*closeTrackingWriter, 0, 3)
	counters := make([]interface{ Add(int64) int64 }, 0, 3)
	flows := make([]*access.Flow, 0, 3)
	for i, u := range users {
		email := userEmail(u.ID)
		links = append(links, openLifecycleLink(t, d, email, "192.0.2.1"))
		c := mustRegisterCounter(t, mgr, counterName(u.ID, "uplink"))
		c.Add(int64((i + 1) * 10))
		counters = append(counters, c)
		_, flow := d.accessContext(context.Background(), xnet.TCPDestination(xnet.DomainAddress("example.test"), 443), email)
		flow.Add(int64((i+1)*10), 0)
		flows = append(flows, flow)
		if lease, rejected := d.checkDeviceLimit(u.UUID, "203.0.113.99", true); !rejected {
			d.delConn(lease)
			t.Fatal("UUID alias bypassed shared gate")
		}
	}
	raw, alive, count, err := x.GetUserTraffic(context.Background())
	if err != nil || len(raw) != 3 || len(alive) != 1 || len(alive[7]) != 1 || count != 3 {
		t.Fatalf("state=%v %v %d %v", raw, alive, count, err)
	}
	for i, u := range users {
		if raw[u.ID][0] != int64((i+1)*10) {
			t.Fatalf("auth counters=%v", raw)
		}
	}
	x.users = []model.UserSpec{users[0], users[2]}
	configureDispatcherLimits(d, x.users)
	counters[1].Add(7)
	flows[1].Add(7, 0)
	raw, alive, _, err = x.GetUserTraffic(context.Background())
	if err != nil || raw[users[1].ID][0] != 27 || len(alive) != 1 {
		t.Fatalf("removed auth tail=%v %v", raw, err)
	}
	totals := make(map[int]int64)
	for _, e := range collector.Drain().Entries {
		totals[e.UserID] += e.Upload
	}
	if len(totals) != 3 || totals[users[1].ID] != 27 {
		t.Fatalf("access identity/tail=%v", totals)
	}
	moved := users[2]
	moved.BillingUserID = 9
	configureDispatcherLimits(d, []model.UserSpec{users[0], moved})
	next := openLifecycleLink(t, d, moved.UUID, "192.0.2.2")
	alive, count = d.GetConnectionState()
	if len(alive) != 2 || !alive[7]["192.0.2.1"] || !alive[9]["192.0.2.2"] || count != 4 {
		t.Fatalf("old lease rebound=%v %d", alive, count)
	}
	next.Close()
	for _, l := range links {
		l.Close()
	}
	assertDispatcherEmpty(t, d)
}

func TestChildIdentityXrayNativeBandwidthUsesAccountBucket(t *testing.T) {
	users := childTestUsers(t)
	for _, proto := range []string{"vless", "http"} {
		t.Run(proto, func(t *testing.T) {
			x, _ := newStatsBackedXray(t, users, nil)
			x.protocol = proto
			bm := featurebandwidth.New()
			if err := x.instance.AddFeature(bm); err != nil {
				t.Fatal(err)
			}
			limits := limiter.New()
			limits.UpdateUsers(users)
			speed := limiter.NewSpeedTracker(limits)
			speed.UpdateBuckets()
			x.SetSpeedLimitFunc(speed.GetLimiter)
			shared := speed.GetLimiter(users[0].UUID)
			for _, u := range users {
				email := userEmail(u.ID)
				if proto == "http" {
					email = u.UUID
				}
				if bm.GetUserLimiter(email) != shared {
					t.Fatalf("native identity %s lost shared limiter", email)
				}
			}
		})
	}
}

func TestChildIdentityXrayNativeSharedListenerAuthentication(t *testing.T) {
	users := childTestUsers(t)
	x, node, target, _ := newHTTPTrafficKernel(t)
	x.Stop()
	if err := x.Start(node, users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	limits := limiter.New()
	limits.UpdateUsers(users)
	speed := limiter.NewSpeedTracker(limits)
	speed.UpdateBuckets()
	x.SetSpeedLimitFunc(speed.GetLimiter)
	x.ConfigureAccess(access.Config{Enabled: true})
	want := make(map[int][2]int64)
	tunnels := make([]*httpTrafficTunnel, 0, len(users))
	for i, u := range users {
		tunnel := openHTTPTrafficTunnel(t, node, target, u)
		tunnels = append(tunnels, tunnel)
		up, down := 100+i*13, 200+i*17
		tunnel.exchange(t, up, down)
		want[u.ID] = [2]int64{int64(up), int64(down)}
	}
	requireHTTPActiveTraffic(t, x, want)
	_, alive, count, err := x.GetUserTraffic(context.Background())
	if err != nil || len(alive) != 1 || len(alive[7]) != 1 || count != 3 {
		t.Fatalf("native account state=%v %d %v", alive, count, err)
	}
	for _, tunnel := range tunnels {
		tunnel.close()
	}
	waitHTTPTrafficIdle(t, x)
	requireHTTPTraffic(t, x, want)
	totals := make(map[int][2]int64)
	for _, entry := range x.DrainAccess().Entries {
		v := totals[entry.UserID]
		v[0] += entry.Upload
		v[1] += entry.Download
		totals[entry.UserID] = v
	}
	if len(totals) != 3 {
		t.Fatalf("native access IDs=%v", totals)
	}
	for id, expected := range want {
		if totals[id] != expected {
			t.Fatalf("native access[%d]=%v want %v", id, totals[id], expected)
		}
	}
}
