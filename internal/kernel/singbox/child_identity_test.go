package singbox

import (
	"context"
	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/limiter"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/tracker"
	"net"
	"strconv"
	"sync"
	"testing"
)

func childTestUsers(t *testing.T) []model.UserSpec {
	t.Helper()
	if strconv.IntSize != 64 {
		t.Skip("child identities require 64-bit ints")
	}
	base := int64(1) << 40
	return []model.UserSpec{{ID: 7, UUID: "11111111-1111-4111-8111-111111111111", SpeedLimit: 8, DeviceLimit: 1}, {ID: int(base + 1), UUID: "22222222-2222-4222-8222-222222222222", BillingUserID: 7, BillingNodeID: 11, SpeedLimit: 8, DeviceLimit: 1}, {ID: int(base + 2), UUID: "33333333-3333-4333-8333-333333333333", BillingUserID: 7, BillingNodeID: 12, SpeedLimit: 8, DeviceLimit: 1}}
}

func TestChildIdentitySingBoxAccountingAndAccountAdmission(t *testing.T) {
	users := childTestUsers(t)
	l := limiter.New()
	l.UpdateUsers(users)
	speed := limiter.NewSpeedTracker(l)
	speed.UpdateBuckets()
	s := New(config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir()})
	s.SetSpeedLimitFunc(speed.GetLimiter)
	s.SetDeviceLimitFunc(l.GetDeviceLimitByUUID)
	if err := s.Start(trafficTestNode(t), users, kernel.TLSCert{}); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	ct := s.connTracker
	ct.access.Configure(access.Config{Enabled: true})
	conns := make([]net.Conn, 0, 3)
	for i, u := range users {
		metadata := accessMetadata()
		metadata.User = u.UUID
		metadata.Source = testInboundContext(u.UUID, "192.0.2.1").Source
		conn := ct.RoutedConnection(context.Background(), &testConn{}, metadata, nil, nil)
		conns = append(conns, conn)
		conn.Write(make([]byte, (i+1)*10))
		if conn.(*trackedConn).limiter != speed.GetLimiter(users[0].UUID) {
			t.Fatal("different account token bucket")
		}
	}
	raw, alive, count := ct.GetUserTraffic()
	if len(raw) != 3 || len(alive) != 1 || len(alive[7]) != 1 || count != 3 {
		t.Fatalf("state=%v %v %d", raw, alive, count)
	}
	for i, u := range users {
		if raw[u.ID][1] != int64((i+1)*10) {
			t.Fatalf("auth traffic=%v", raw)
		}
	}
	for _, u := range users {
		denied := &testConn{}
		if got := ct.RoutedConnection(context.Background(), denied, testInboundContext(u.UUID, "203.0.113.99"), nil, nil); got != denied || !denied.closed {
			t.Fatal("alias bypassed account devices")
		}
	}
	// UDP keeps its existing protocol behavior while joining the same device gate.
	packet := &accessPacketConn{}
	if got := ct.RoutedPacketConnection(context.Background(), packet, testInboundContext(users[2].UUID, "203.0.113.99"), nil, nil); got != packet {
		t.Fatal("UDP alias bypassed account devices")
	}
	tr := tracker.New()
	tr.Process(raw, alive, count)
	if _, err := s.RemoveUsers(users[1:2]); err != nil {
		t.Fatal(err)
	}
	conns[1].Write(make([]byte, 7))
	conns[1].Close()
	raw, alive, count = ct.GetUserTraffic()
	tr.Process(raw, alive, count)
	if got := tr.FlushTraffic()[users[1].ID][1]; got != 27 {
		t.Fatalf("removed auth tail=%d", got)
	}
	entries := ct.access.Drain().Entries
	totals := make(map[int]int64)
	for _, e := range entries {
		totals[e.UserID] += e.Download
	}
	if len(totals) != 3 || totals[users[1].ID] != 27 {
		t.Fatalf("access identity/tail=%v", totals)
	}
	// Metadata-only delta updates must rebind new connections without moving old leases.
	moved := users[2]
	moved.BillingUserID = 9
	if _, err := s.AddUsers([]model.UserSpec{moved}); err != nil {
		t.Fatal(err)
	}
	l.UpdateUsers([]model.UserSpec{users[0], moved})
	speed.UpdateBuckets()
	extra := ct.RoutedConnection(context.Background(), &testConn{}, testInboundContext(moved.UUID, "192.0.2.2"), nil, nil)
	_, alive, count = ct.GetUserTraffic()
	if !alive[7]["192.0.2.1"] || !alive[9]["192.0.2.2"] || count != 3 {
		t.Fatalf("rebind moved old leases: %v %d", alive, count)
	}
	extra.Close()
	for _, c := range conns {
		c.Close()
	}
	if ct.ActiveCount() != 0 {
		t.Fatal("account connections leaked")
	}
	// sync.devices arrives under the real account ID, including with no parent identity.
	ct.UpdateGlobalDevices(map[int][]string{9: {"192.0.2.1"}})
	denied := &testConn{}
	ct.RoutedConnection(context.Background(), denied, testInboundContext(moved.UUID, "203.0.113.99"), nil, nil)
	if !denied.closed {
		t.Fatal("virtual ID ignored real-account sync.devices")
	}
}

func TestChildIdentitySingBoxConcurrentGateAndAccountKick(t *testing.T) {
	users := childTestUsers(t)
	ct := NewConnTracker(0)
	ct.SetUserMap(buildUserMap(users), buildAccountMap(users))
	ct.SetDeviceLimitFunc(func(string) (int, bool) { return 1, true })
	first := ct.RoutedConnection(context.Background(), &accessConcurrentConn{}, testInboundContext(users[0].UUID, "192.0.2.1"), nil, nil)
	defer first.Close()
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := ct.RoutedConnection(context.Background(), &accessConcurrentConn{}, testInboundContext(users[i%3].UUID, "192.0.2.1"), nil, nil)
			defer c.Close()
		}(i)
	}
	wg.Wait()
	a := ct.RoutedConnection(context.Background(), &testConn{}, testInboundContext(users[1].UUID, "192.0.2.1"), nil, nil)
	b := ct.RoutedConnection(context.Background(), &testConn{}, testInboundContext(users[2].UUID, "192.0.2.1"), nil, nil)
	defer a.Close()
	defer b.Close()
	if got := ct.CloseByUUID(users[1].UUID); got != 1 {
		t.Fatalf("alias kick=%d", got)
	}
	if got := ct.CloseByUUID(users[0].UUID); got != 2 || ct.ActiveCount() != 0 {
		t.Fatalf("account kick=%d active=%d", got, ct.ActiveCount())
	}
}

func TestChildIdentitySingBoxNaivePreservesNumericUsername(t *testing.T) {
	users := childTestUsers(t)
	ct := NewConnTracker(0)
	ct.SetUserMap(buildUserMap(users), buildAccountMap(users))
	limits := limiter.New()
	limits.UpdateUsers(users)
	speed := limiter.NewSpeedTracker(limits)
	speed.UpdateBuckets()
	ct.SetSpeedLimitFunc(speed.GetLimiter)
	ct.SetDeviceLimitFunc(limits.GetDeviceLimitByUUID)
	ct.access.Configure(access.Config{Enabled: true})
	first := ct.RoutedConnection(context.Background(), &testConn{}, testInboundContext(strconv.Itoa(users[1].ID), "192.0.2.1"), nil, nil)
	defer first.Close()
	first.Write([]byte("naive"))
	if first.(*trackedConn).limiter != speed.GetLimiter(users[0].UUID) {
		t.Fatal("numeric Naive username lost the shared limiter")
	}
	denied := &testConn{}
	ct.RoutedConnection(context.Background(), denied, testInboundContext(strconv.Itoa(users[2].ID), "203.0.113.99"), nil, nil)
	if !denied.closed {
		t.Fatal("numeric Naive username bypassed account devices")
	}
	raw, alive, count := ct.GetUserTraffic()
	if raw[users[1].ID][1] != 5 || len(raw) != 1 || len(alive) != 1 || count != 1 {
		t.Fatalf("numeric auth accounting=%v %v %d", raw, alive, count)
	}
	entries := ct.access.Drain().Entries
	if len(entries) != 1 || entries[0].UserID != users[1].ID {
		t.Fatalf("numeric access IDs=%v", entries)
	}
}
