package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/model"
)

type accessTestKernel struct {
	fakeDebugKernel
	snapshot access.Snapshot
	config   access.Config
}

func (k *accessTestKernel) ConfigureAccess(cfg access.Config) { k.config = cfg }
func (k *accessTestKernel) DrainAccess() access.Snapshot {
	v := k.snapshot
	k.snapshot = access.Snapshot{}
	return v
}
func destinationEntry(uid int, u, d int64) access.Entry {
	now := time.Now().Unix()
	return access.Entry{UserID: uid, Domain: "video.example", Port: 443, Network: "tcp", DomainSource: "request", BucketAt: now / 300 * 300, FirstSeen: now, LastSeen: now, Upload: u, Download: d, Connections: 1}
}

func TestAccessBudgetsCarryExcessAndRepresentMissingBytes(t *testing.T) {
	s := reportService(controlplane.NewPanelControlPlane(config.PanelConfig{}, config.WSConfig{}, config.KernelConfig{}))
	k := &accessTestKernel{fakeDebugKernel: fakeDebugKernel{name: "sing-box"}, snapshot: access.Snapshot{Entries: []access.Entry{destinationEntry(1, 100, 200)}}}
	s.kernel = k
	s.configureAccess(&model.NodeSpec{AccessLog: &access.Config{Enabled: true, BucketSeconds: 300, MaxEntries: 2000}})
	first := s.prepareAccessReport("billing-1", map[int][2]int64{1: {60, 150}, 2: {5, 10}})
	if first == nil || first.Coverage != "partial" {
		t.Fatalf("first=%+v", first)
	}
	if err := validateAccessReport(*first); err != nil {
		t.Fatal(err)
	}
	if len(s.accessPending) != 1 || s.accessPending[0].Upload != 40 || s.accessPending[0].Download != 50 {
		t.Fatalf("carry=%+v", s.accessPending)
	}
	if first.Entries[0].Upload != 60 || first.Entries[0].Download != 150 || first.Entries[1].Domain != "" || first.Entries[1].UserID != 2 {
		t.Fatalf("entries=%+v", first.Entries)
	}
	second := s.prepareAccessReport("billing-2", map[int][2]int64{1: {40, 50}})
	if err := validateAccessReport(*second); err != nil {
		t.Fatal(err)
	}
	if len(s.accessPending) != 0 || second.Entries[0].Connections != 0 || second.Coverage != "complete" {
		t.Fatalf("second=%+v", second)
	}
}

func TestAccessFailureDoesNotBlockBillingAndReplayKeepsPayload(t *testing.T) {
	var mu sync.Mutex
	detailFailed := true
	var details []controlplane.AccessReport
	var billingIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/v2/server/access/report" {
			var report controlplane.AccessReport
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Error(err)
				return
			}
			details = append(details, report)
			if detailFailed {
				w.WriteHeader(503)
				return
			}
		} else {
			var report struct {
				ReportID string `json:"report_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Error(err)
				return
			}
			billingIDs = append(billingIDs, report.ReportID)
		}
		w.Write([]byte("{\"data\":true}"))
	}))
	defer server.Close()
	dir := t.TempDir()
	s := durableService(t, dir, server.URL)
	s.kernel = &accessTestKernel{fakeDebugKernel: fakeDebugKernel{name: "sing-box"}, snapshot: access.Snapshot{Entries: []access.Entry{destinationEntry(1, 100, 200)}}}
	s.configureAccess(&model.NodeSpec{AccessLog: &access.Config{Enabled: true, BucketSeconds: 300, MaxEntries: 2000}})
	s.tracker.Process(map[int][2]int64{1: {100, 200}}, nil, 0)
	if err := s.sendReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.sendAccessReport(context.Background()); err == nil {
		t.Fatal("expected detail failure")
	}
	s.tracker.Process(map[int][2]int64{1: {110, 220}}, nil, 0)
	if err := s.sendReport(context.Background()); err != nil {
		t.Fatalf("detail failure blocked billing: %v", err)
	}
	restored := durableService(t, dir, server.URL)
	mu.Lock()
	detailFailed = false
	mu.Unlock()
	if err := restored.flushAccessReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(billingIDs) != 2 || len(details) != 3 || !reflect.DeepEqual(details[0], details[1]) {
		t.Fatalf("billing=%v detail=%+v", billingIDs, details)
	}
	if details[0].BillingReportID != billingIDs[0] || details[2].BillingReportID != billingIDs[1] {
		t.Fatal("lost billing linkage")
	}
}

func TestCorruptDetailSpoolDoesNotBlockNewBillingAndDeferredRecovers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{\"data\":true}")) }))
	defer server.Close()
	dir := t.TempDir()
	s := durableService(t, dir, server.URL)
	s.kernel = &accessTestKernel{fakeDebugKernel: fakeDebugKernel{name: "sing-box"}, snapshot: access.Snapshot{Entries: []access.Entry{destinationEntry(1, 10, 20)}}}
	s.configureAccess(&model.NodeSpec{AccessLog: &access.Config{Enabled: true, BucketSeconds: 300, MaxEntries: 2000}})
	path, _, err := s.accessSpoolLocation()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt detail spool"), 0600); err != nil {
		t.Fatal(err)
	}
	s.tracker.Process(map[int][2]int64{1: {10, 20}}, nil, 0)
	if err := s.sendReport(context.Background()); err != nil {
		t.Fatalf("detail corruption blocked billing: %v", err)
	}
	s.tracker.Process(map[int][2]int64{1: {20, 40}}, nil, 0)
	if err := s.sendReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.pendingReport != nil || len(s.deferredAccess) != 2 {
		t.Fatalf("billing pending=%+v deferred=%d", s.pendingReport, len(s.deferredAccess))
	}
	restored := durableService(t, dir, server.URL)
	if err := restored.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	if len(restored.deferredAccess) != 2 {
		t.Fatal("deferred details were not persisted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	restored.retryDeferredAccess()
	if len(restored.deferredAccess) != 0 || len(restored.accessQueue) != 2 {
		t.Fatal("details did not recover after spool repair")
	}
	if err := restored.flushAccessReports(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredDetailsRespectByteBudget(t *testing.T) {
	s := reportService(nil)
	entries := make([]access.Entry, 10000)
	domain := strings.Repeat("a", 63) + "." + strings.Repeat("b", 63) + "." + strings.Repeat("c", 63) + "." + strings.Repeat("d", 60)
	for i := range entries {
		entries[i] = destinationEntry(1, 1, 1)
		entries[i].Domain = domain
	}
	for _, id := range []string{"one", "two", "three", "four"} {
		s.deferAccessReport(controlplane.AccessReport{Version: 1, ReportID: id, BillingReportID: id, Kernel: "sing-box", ObservedAt: time.Now().Unix(), Coverage: "complete", Totals: map[int][2]int64{1: {10000, 10000}}, Entries: entries})
	}
	encoded, err := json.Marshal(s.deferredAccess)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > maxDeferredAccessBytes || len(s.deferredAccess) == 4 || s.accessDropped == 0 {
		t.Fatalf("deferred bytes=%d batches=%d dropped=%d", len(encoded), len(s.deferredAccess), s.accessDropped)
	}
}

func TestExpiredAccessHeadDoesNotBlockFreshDetails(t *testing.T) {
	var ids []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var report controlplane.AccessReport
		if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
			t.Error(err)
			return
		}
		ids = append(ids, report.ReportID)
		if report.ReportID == "expired" {
			w.WriteHeader(http.StatusGone)
			return
		}
		w.Write([]byte("{\"data\":true}"))
	}))
	defer server.Close()
	s := durableService(t, t.TempDir(), server.URL)
	for _, id := range []string{"expired", "fresh"} {
		report := controlplane.AccessReport{Version: 1, ReportID: id, BillingReportID: id, Kernel: "sing-box", ObservedAt: time.Now().Unix(), Coverage: "complete", Totals: map[int][2]int64{1: {10, 20}}, Entries: []access.Entry{destinationEntry(1, 10, 20)}}
		if err := s.enqueueAccessReport(report); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.flushAccessReports(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(ids, []string{"expired", "fresh"}) || len(s.accessQueue) != 0 || s.accessDropped != 1 {
		t.Fatalf("sent=%v queue=%d dropped=%d", ids, len(s.accessQueue), s.accessDropped)
	}
}

func TestAccessStagingSurvivesLostBillingAck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer server.Close()
	dir := t.TempDir()
	s := durableService(t, dir, server.URL)
	s.kernel = &accessTestKernel{fakeDebugKernel: fakeDebugKernel{name: "sing-box"}, snapshot: access.Snapshot{Entries: []access.Entry{destinationEntry(1, 10, 20)}}}
	s.configureAccess(&model.NodeSpec{AccessLog: &access.Config{Enabled: true, BucketSeconds: 300, MaxEntries: 2000}})
	s.tracker.Process(map[int][2]int64{1: {10, 20}}, nil, 0)
	if err := s.sendReport(context.Background()); err == nil {
		t.Fatal("expected billing failure")
	}
	restored := durableService(t, dir, server.URL)
	if err := restored.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	if restored.pendingReport.Access == nil || !reflect.DeepEqual(restored.pendingReport.Access, s.pendingReport.Access) {
		t.Fatal("staged detail was lost")
	}
}
