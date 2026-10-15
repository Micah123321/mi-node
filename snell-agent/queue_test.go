package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReportRetryStableIDAndRestartRecovery(t *testing.T) {
	f := newFixture(t)
	cfg := fixtureConfig(t, f)
	a, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	f.failures = 1
	f.mu.Unlock()
	a.stats.add("1", 11, 27)
	if e = a.report(context.Background()); e == nil {
		t.Fatal("expected lost acknowledgement")
	}
	first, ok := a.queue.first()
	if !ok {
		t.Fatal("report not retained")
	}
	f.mu.Lock()
	original := append([]byte(nil), f.bodies[0]...)
	f.ack = `{"data":false}`
	f.mu.Unlock()
	a.stats.add("1", 5, 9)
	if e = a.report(context.Background()); e == nil {
		t.Fatal("false ack discarded report")
	}
	second, _ := a.queue.first()
	if first.ID != second.ID {
		t.Fatal("retry ID changed")
	}
	f.mu.Lock()
	if !bytes.Equal(original, f.bodies[1]) {
		t.Error("retry body changed")
	}
	f.mu.Unlock()
	// A normal shutdown appends remaining deltas, without rewriting pending IDs.
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(filepath.Join(cfg.StateDir, "reports.json"))
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(b), cfg.Token) || strings.Contains(string(b), f.users[0].UUID) {
		t.Fatal("credentials persisted in report queue")
	}
	restarted, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	defer restarted.Close()
	recovered, ok := restarted.queue.first()
	if !ok || recovered.ID != first.ID {
		t.Fatal("restart did not recover original report")
	}
	f.mu.Lock()
	f.ack = ""
	f.mu.Unlock()
	if e = restarted.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	if _, ok = restarted.queue.first(); ok {
		t.Fatal("acknowledged reports remain")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.totals["1"] != [2]int64{16, 36} {
		t.Fatalf("cumulative traffic duplicated: %v", f.totals)
	}
	if !bytes.Equal(original, f.bodies[2]) {
		t.Fatal("restart retry changed original bytes")
	}
}
func TestReportCrashRecoveryFromCommittedDiskBatch(t *testing.T) {
	f := newFixture(t)
	cfg := fixtureConfig(t, f)
	a, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	a.stats.add("8", 100, 200)
	f.mu.Lock()
	f.failures = 1
	f.mu.Unlock()
	if a.report(context.Background()) == nil {
		t.Fatal("expected failure")
	}
	original, _ := a.queue.first()
	// Simulate process exit after checkpoint, before ack or graceful Close.
	a.cancel()
	a.queue.close()
	recovered, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	defer recovered.Close()
	r, ok := recovered.queue.first()
	if !ok || r.ID != original.ID {
		t.Fatal("committed report lost")
	}
	if e = recovered.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.totals["8"] != [2]int64{100, 200} {
		t.Fatal("retry was double billed")
	}
}
func TestCheckpointFailurePreservesDeltasAndClosesAccess(t *testing.T) {
	f := newFixture(t)
	a := newTestAgent(t, f)
	target := echoServer(t)
	s := dialStream(t, a, f.users[0].UUID, target, "", false)
	if e := exchange(s, []byte("count-me")); e != nil {
		t.Fatal(e)
	}
	original := a.queue.path
	a.queue.path = filepath.Join(a.cfg.StateDir, "missing", "reports.json")
	if e := a.report(context.Background()); e == nil {
		t.Fatal("checkpoint failure hidden")
	}
	assertClosed(t, s)
	if len(a.ListUsers()) != 0 {
		t.Fatal("disk failure did not fail closed")
	}
	a.stats.mu.Lock()
	v := a.stats.traffic["1"]
	a.stats.mu.Unlock()
	if v != [2]int64{8, 8} {
		t.Fatalf("uncommitted deltas lost: %v", v)
	}
	a.queue.path = original
	if e := a.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	if len(a.ListUsers()) != 2 {
		t.Fatal("disk recovery did not restore fresh snapshot")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.totals["1"] != [2]int64{8, 8} {
		t.Fatal("disk retry changed traffic")
	}
}
func TestAckPersistenceFailureRetriesOriginalBatch(t *testing.T) {
	f := newFixture(t)
	cfg := fixtureConfig(t, f)
	a, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	a.stats.add("1", 9, 19)
	if e = a.checkpoint(); e != nil {
		t.Fatal(e)
	}
	r, _ := a.queue.first()
	if e = a.panel.report(context.Background(), r); e != nil {
		t.Fatal(e)
	}
	original := a.queue.path
	a.queue.path = filepath.Join(cfg.StateDir, "missing", "reports.json")
	if a.queue.ack(r.ID) == nil {
		t.Fatal("expected ack persistence failure")
	}
	still, _ := a.queue.first()
	if still.ID != r.ID {
		t.Fatal("failed ack removed batch")
	}
	a.queue.path = original
	if e = a.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.totals["1"] != [2]int64{9, 19} {
		t.Fatal("ack retry duplicated billing")
	}
}
func TestQueueLockIdentityAndCorruption(t *testing.T) {
	f := newFixture(t)
	cfg := fixtureConfig(t, f)
	a, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	if other, e := New(cfg, quiet); e == nil {
		other.Close()
		t.Fatal("state directory allowed concurrent writers")
	}
	a.stats.add("1", 1, 2)
	if e = a.Close(); e != nil {
		t.Fatal(e)
	}
	changed := cfg
	changed.NodeID++
	if b, e := New(changed, quiet); e == nil {
		b.Close()
		t.Fatal("queue accepted different node")
	}
	changed = cfg
	changed.PanelURL += "/other"
	if b, e := New(changed, quiet); e == nil {
		b.Close()
		t.Fatal("queue accepted different panel")
	}
	path := filepath.Join(cfg.StateDir, "reports.json")
	if e = os.WriteFile(path, []byte("{bad JSON"), 0600); e != nil {
		t.Fatal(e)
	}
	if b, e := New(cfg, quiet); e == nil {
		b.Close()
		t.Fatal("corrupt queue was discarded")
	}
}
func TestGracefulRunPersistsFinalTraffic(t *testing.T) {
	f := newFixture(t)
	cfg := fixtureConfig(t, f)
	cfg.PullInterval = Duration(time.Hour)
	cfg.ReportInterval = Duration(time.Hour)
	a, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()
	eventually(t, func() bool { return len(a.ListUsers()) == 2 })
	target := echoServer(t)
	s := dialStream(t, a, f.users[0].UUID, target, "", true)
	if e = exchange(s, []byte("shutdown-delta")); e != nil {
		t.Fatal(e)
	}
	cancel()
	select {
	case e = <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown stalled")
	}
	assertClosed(t, s)
	restored, e := New(cfg, quiet)
	if e != nil {
		t.Fatal(e)
	}
	defer restored.Close()
	if e = restored.report(context.Background()); e != nil {
		t.Fatal(e)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.totals["1"] != [2]int64{14, 14} {
		t.Fatalf("final traffic missing: %v", f.totals)
	}
}
func TestQueueSnapshotOnDiskContainsOnlyDeltaBatches(t *testing.T) {
	f := newFixture(t)
	a, e := New(fixtureConfig(t, f), quiet)
	if e != nil {
		t.Fatal(e)
	}
	defer a.Close()
	a.stats.add("1", 3, 7)
	if e = a.checkpoint(); e != nil {
		t.Fatal(e)
	}
	a.stats.add("1", 2, 5)
	if e = a.checkpoint(); e != nil {
		t.Fatal(e)
	}
	b, e := os.ReadFile(a.queue.path)
	if e != nil {
		t.Fatal(e)
	}
	var s diskState
	if e = json.Unmarshal(b, &s); e != nil {
		t.Fatal(e)
	}
	if len(s.Pending) != 2 || s.Pending[0].Traffic["1"] != [2]int64{3, 7} || s.Pending[1].Traffic["1"] != [2]int64{2, 5} {
		t.Fatalf("not incremental: %+v", s)
	}
}
