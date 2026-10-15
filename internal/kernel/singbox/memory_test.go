package singbox

import (
	"context"
	"fmt"
	"net"
	"runtime"
	"sync"
	"testing"
)

func TestUserStatsReleasesIdleIPMap(t *testing.T) {
	us := &userStats{}
	for i := 0; i < 1000; i++ {
		us.addConn(fmt.Sprint(i))
	}
	for i := 0; i < 1000; i++ {
		us.removeConn(fmt.Sprint(i))
	}
	if us.connCount != 0 || us.ips != nil {
		t.Fatal("idle user retained peak IP map capacity")
	}
	us.addConn("next")
	if us.connCount != 1 || us.ips["next"] != 1 {
		t.Fatal("connection after idle not tracked")
	}
}

func TestConnTrackerSnapshotSizesFollowActivity(t *testing.T) {
	tr := NewConnTracker(0)
	tr.SetUserMap(map[string]int{"one": 1, "two": 2})
	tr.users[1].addConn("192.0.2.1")
	tr.users[1].upload.Store(10)
	tr.GetUserTraffic()
	tr.users[2].addConn("192.0.2.2")
	tr.users[2].download.Store(20)
	traffic, alive, count := tr.GetUserTraffic()
	if len(traffic) != 2 || len(alive) != 2 || count != 2 {
		t.Fatalf("growth lost state: %v %v %d", traffic, alive, count)
	}
	tr.users[1].removeConn("192.0.2.1")
	tr.users[2].removeConn("192.0.2.2")
	traffic, alive, count = tr.GetUserTraffic()
	if len(traffic) != 2 || len(alive) != 0 || count != 0 {
		t.Fatalf("idle cleared accounting: %v %v %d", traffic, alive, count)
	}
	if tr.lastTrafficUsers.Load() != 2 || tr.lastOnlineUsers.Load() != 0 {
		t.Fatal("snapshot hints did not shrink with online users")
	}
}

func TestConnTrackerPeakRecoveryPreservesSurvivingConnections(t *testing.T) {
	tr := NewConnTracker(0)
	tr.SetUserMap(map[string]int{"one": 1})
	conns := make([]net.Conn, 1024)
	for i := range conns {
		conns[i] = tr.RoutedConnection(context.Background(), &testConn{}, testInboundContext("one", "192.0.2.1"), nil, nil)
	}
	for _, conn := range conns[:1000] {
		conn.Close()
	}
	traffic, alive, count := tr.GetUserTraffic()
	if count != 24 || len(tr.connMap) != 24 || !alive[1]["192.0.2.1"] {
		t.Fatalf("peak shrink lost live connections: %v %d", alive, count)
	}
	conns[1000].Write([]byte("kept"))
	traffic, _, _ = tr.GetUserTraffic()
	if traffic[1][1] != 4 {
		t.Fatal("surviving connection lost accounting")
	}
	if !tr.CloseByID(conns[1000].(*trackedConn).connID) {
		t.Fatal("surviving connection missing from force-close index")
	}
	for _, conn := range conns[1000:] {
		conn.Close()
	}
	if tr.ActiveCount() != 0 || len(tr.connMap) != 0 {
		t.Fatal("closed connections remain tracked")
	}
}

func TestConnTrackerPeakRecoveryWithConcurrentAdmissions(t *testing.T) {
	tr := NewConnTracker(0)
	tr.SetUserMap(map[string]int{"one": 1})
	conns := make([]net.Conn, 2048)
	for i := range conns {
		conns[i] = tr.RoutedConnection(context.Background(), &accessConcurrentConn{}, testInboundContext("one", "192.0.2.1"), nil, nil)
	}
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := worker; i < len(conns); i += 8 {
				conns[i].Write([]byte("x"))
				conns[i].Close()
			}
		}()
	}
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 512; i++ {
			conn := tr.RoutedConnection(context.Background(), &accessConcurrentConn{}, testInboundContext("one", "192.0.2.2"), nil, nil)
			conn.Write([]byte("x"))
			conn.Close()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			tr.GetUserTraffic()
		}
	}()
	wg.Wait()
	traffic, alive, count := tr.GetUserTraffic()
	if traffic[1][1] != 2560 || count != 0 || len(alive) != 0 || len(tr.connMap) != 0 {
		t.Fatalf("concurrent peak recovery changed accounting or lifecycle: %v %v %d", traffic, alive, count)
	}
}

func BenchmarkConnTrackerPeakRecovery(b *testing.B) {
	for b.Loop() {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		tr := NewConnTracker(0)
		tr.SetUserMap(map[string]int{"one": 1})
		conns := make([]net.Conn, 50000)
		for i := range conns {
			conns[i] = tr.RoutedConnection(context.Background(), &testConn{}, testInboundContext("one", "192.0.2.1"), nil, nil)
		}
		for i := range conns {
			conns[i].Close()
			conns[i] = nil
		}
		conns = nil
		runtime.GC()
		runtime.ReadMemStats(&after)
		if after.HeapAlloc > before.HeapAlloc {
			b.ReportMetric(float64(after.HeapAlloc-before.HeapAlloc), "retained-B")
		}
		runtime.KeepAlive(tr)
	}
}

func BenchmarkConnTrackerActiveUsers(b *testing.B) {
	tr := NewConnTracker(0)
	users := make(map[string]int, 10000)
	for i := 1; i <= 10000; i++ {
		users[fmt.Sprint(i)] = i
	}
	tr.SetUserMap(users)
	for _, us := range tr.users {
		us.upload.Store(100)
		us.addConn("192.0.2.1")
	}
	tr.GetUserTraffic()
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		tr.GetUserTraffic()
	}
}

func BenchmarkConnTrackerIdleUsers(b *testing.B) {
	users := make(map[string]int, 10000)
	for i := 1; i <= 10000; i++ {
		users[fmt.Sprint(i)] = i
	}
	b.Run("Configure", func(b *testing.B) {
		var tr *ConnTracker
		b.ReportAllocs()
		for b.Loop() {
			tr = NewConnTracker(0)
			tr.SetUserMap(users)
		}
		runtime.KeepAlive(tr)
	})
	b.Run("Sample", func(b *testing.B) {
		tr := NewConnTracker(0)
		tr.SetUserMap(users)
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			tr.GetUserTraffic()
		}
	})
}
