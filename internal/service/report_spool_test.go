package service

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/controlplane"
)

func durableService(t *testing.T, dir, url string) *Service {
	t.Helper()
	pc := config.PanelConfig{URL: url, NodeID: 1}
	s := reportService(controlplane.NewPanelControlPlane(pc, config.WSConfig{}, config.KernelConfig{}))
	s.cfg = &config.Config{Panel: pc, Kernel: config.KernelConfig{ConfigDir: dir}}
	return s
}

func TestSpoolShutdownRecoveryLostResponseAndTail(t *testing.T) {
	var mu sync.Mutex
	offline := true
	var calls []spoolBatch
	seen := make(map[string]bool)
	var charged int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b spoolBatch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		calls = append(calls, b)
		if !seen[b.ReportID] {
			seen[b.ReportID] = true
			charged += b.Traffic[1][0]
		}
		fail := offline
		mu.Unlock()
		if fail {
			c, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			c.Close()
			return
		}
		w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()
	dir := t.TempDir()
	s := durableService(t, dir, server.URL)
	s.tracker.Process(map[int][2]int64{1: {100, 20}}, nil, 0)
	if err := s.sendReport(context.Background()); err == nil {
		t.Fatal("expected lost response")
	}
	id := s.pendingReport.ReportID
	s.kernel = &finalTrafficKernel{fakeDebugKernel: fakeDebugKernel{running: true}, tail: map[int][2]int64{1: {175, 30}}}
	if err := s.shutdownReports(); err == nil {
		t.Fatal("expected offline shutdown")
	}
	restored := durableService(t, dir, server.URL)
	if err := restored.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	if len(restored.reportQueue) != 2 || restored.pendingReport.ReportID != id {
		t.Fatalf("recovered queue=%+v", restored.reportQueue)
	}
	tail := restored.reportQueue[1]
	if tail.ReportID == id || tail.Traffic[1] != [2]int64{75, 10} {
		t.Fatalf("tail=%+v", tail)
	}
	mu.Lock()
	offline = false
	mu.Unlock()
	for restored.pendingReport != nil {
		if err := restored.sendReport(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	again := durableService(t, dir, server.URL)
	if err := again.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	if len(again.reportQueue) != 0 {
		t.Fatal("ACK did not clear queue")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(calls) != 4 || !reflect.DeepEqual(calls[0], calls[1]) || !reflect.DeepEqual(calls[0], calls[2]) || charged != 175 {
		t.Fatalf("calls=%+v charged=%d", calls, charged)
	}
}

func TestSpoolCorruptionAndIdentityPreserved(t *testing.T) {
	dir := t.TempDir()
	s := durableService(t, dir, "https://panel.example")
	s.tracker.Process(map[int][2]int64{1: {100, 20}}, nil, 0)
	if err := s.prepareReport(); err != nil {
		t.Fatal(err)
	}
	path, _, _ := s.spoolLocation()
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*config.Config){func(c *config.Config) { c.Panel.NodeID++ }, func(c *config.Config) { c.Panel.URL += "/other" }} {
		other := durableService(t, dir, s.cfg.Panel.URL)
		mutate(other.cfg)
		if err := other.loadReportSpool(); err != nil {
			t.Fatal(err)
		}
		if other.pendingReport != nil {
			t.Fatal("cross-identity restore")
		}
		otherPath, _, _ := other.spoolLocation()
		if err := os.WriteFile(otherPath, original, 0600); err != nil {
			t.Fatal(err)
		}
		other.spoolLoaded = false
		if err := other.loadReportSpool(); err == nil {
			t.Fatal("copied foreign spool accepted")
		}
	}
	var valid reportSpool
	if err := json.Unmarshal(original, &valid); err != nil {
		t.Fatal(err)
	}
	valid.Batches[0].Traffic[1] = [2]int64{999, 20}
	tampered, _ := json.Marshal(valid)
	for _, data := range [][]byte{[]byte("{"), tampered} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		fresh := durableService(t, dir, s.cfg.Panel.URL)
		if err := fresh.Run(context.Background()); err == nil {
			t.Fatal("corrupt spool accepted")
		}
		got, _ := os.ReadFile(path)
		if string(got) != string(data) {
			t.Fatal("corrupt spool modified")
		}
	}
}

func TestSpoolWriteFailureRetainsBatchWithoutSending(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.Write([]byte(`{"data":true}`)) }))
	defer server.Close()
	s := durableService(t, t.TempDir(), server.URL)
	if err := s.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	path, _, _ := s.spoolLocation()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(path, "block"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s.tracker.Process(map[int][2]int64{1: {100, 20}}, nil, 0)
	if err := s.sendReport(context.Background()); err == nil {
		t.Fatal("expected rename failure")
	}
	if calls != 0 || s.pendingReport == nil {
		t.Fatal("sent or discarded unsaved batch")
	}
	id := s.pendingReport.ReportID
	s.kernel = &finalTrafficKernel{tail: map[int][2]int64{1: {175, 30}}}
	if err := s.shutdownReports(); err == nil {
		t.Fatal("expected disk error")
	}
	if len(s.reportQueue) != 2 || s.reportQueue[1].Traffic[1] != [2]int64{75, 10} {
		t.Fatal("tail lost on disk failure")
	}
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	if err := s.persistReportQueue(s.reportQueue); err != nil {
		t.Fatal(err)
	}
	fresh := durableService(t, s.cfg.Kernel.ConfigDir, server.URL)
	if err := fresh.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	if fresh.pendingReport.ReportID != id || len(fresh.reportQueue) != 2 {
		t.Fatal("batch identity changed after disk recovery")
	}
}

func TestSpoolACKUpdateFailureReplaysSameID(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	saved := dir + "-saved"
	var failErr error
	var got spoolBatch
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		// Make only the ACK update fail; the pre-send durable file remains intact.
		failErr = os.Rename(dir, saved)
		if failErr == nil {
			failErr = os.WriteFile(dir, []byte("blocked"), 0600)
		}
		w.Write([]byte(`{"data":true}`))
	}))
	s := durableService(t, dir, server.URL)
	s.tracker.Process(map[int][2]int64{1: {100, 20}}, nil, 0)
	err := s.sendReport(context.Background())
	server.Close()
	if failErr != nil {
		t.Fatal(failErr)
	}
	if err == nil || s.pendingReport == nil {
		t.Fatal("ACK update failure discarded batch")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(saved, dir); err != nil {
		t.Fatal(err)
	}
	fresh := durableService(t, dir, server.URL)
	if err := fresh.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	if fresh.pendingReport == nil || fresh.pendingReport.ReportID != got.ReportID || !reflect.DeepEqual(fresh.pendingReport.Traffic, got.Traffic) {
		t.Fatal("ACK failure changed recovered batch")
	}
}

// Reproduce a0499b9 independently of the current spool types and checksum.
type legacySpoolBatch struct {
	ReportID string           `json:"report_id"`
	Traffic  map[int][2]int64 `json:"traffic"`
}

type legacyReportSpool struct {
	Version  int                `json:"version"`
	Identity string             `json:"identity"`
	Batches  []legacySpoolBatch `json:"batches"`
	Checksum string             `json:"checksum"`
}

func spoolTestJSON(t *testing.T, v any) []byte {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func legacySpoolChecksum(t *testing.T, v legacyReportSpool) string {
	t.Helper()
	v.Checksum = ""
	return fmt.Sprintf("%x", sha256.Sum256(spoolTestJSON(t, v)))
}

func spoolTestAccessReport(id string) controlplane.AccessReport {
	return controlplane.AccessReport{Version: 1, ReportID: "access-" + id, BillingReportID: id, Kernel: "sing-box", ObservedAt: 1700000100, Coverage: "complete", Totals: map[int][2]int64{1: {10, 20}}, Entries: []access.Entry{destinationEntry(1, 10, 20)}}
}

func TestSpoolLegacyCacheRestores(t *testing.T) {
	for _, tc := range []struct {
		name    string
		batches []legacySpoolBatch
	}{
		{name: "null batches"},
		{name: "empty batches", batches: []legacySpoolBatch{}},
		{name: "pending billing", batches: []legacySpoolBatch{
			{ReportID: "billing-1", Traffic: map[int][2]int64{1: {100, 200}, 10: {3, 4}}},
			{ReportID: "billing-2", Traffic: map[int][2]int64{1: {5, 6}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := durableService(t, t.TempDir(), "https://panel.example")
			path, identity, err := s.spoolLocation()
			if err != nil {
				t.Fatal(err)
			}
			v := legacyReportSpool{Version: 1, Identity: identity, Batches: tc.batches}
			v.Checksum = legacySpoolChecksum(t, v)
			if err := os.WriteFile(path, spoolTestJSON(t, v), 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.loadReportSpool(); err != nil {
				t.Fatal(err)
			}
			if !s.spoolLoaded || len(s.reportQueue) != len(tc.batches) || len(s.deferredAccess) != 0 {
				t.Fatalf("legacy cache recovery: queue=%+v deferred=%+v", s.reportQueue, s.deferredAccess)
			}
			for i, b := range tc.batches {
				p := s.reportQueue[i]
				if p.ReportID != b.ReportID || !reflect.DeepEqual(p.Traffic, b.Traffic) || p.Access != nil {
					t.Fatalf("legacy batch %d changed: %+v", i, p)
				}
			}
			if len(tc.batches) > 0 && s.pendingReport != s.reportQueue[0] {
				t.Fatal("legacy pending batch was not restored")
			}
		})
	}
}

func TestSpoolLegacyReaderAcceptsAccessExtensions(t *testing.T) {
	for _, name := range []string{"staged and deferred", "deferred only", "billing only"} {
		t.Run(name, func(t *testing.T) {
			s := durableService(t, t.TempDir(), "https://panel.example")
			staged := spoolTestAccessReport("billing-1")
			queue := []*controlplane.ReportPayload{
				{ReportID: "billing-1", Traffic: staged.Totals, Access: &staged},
				{ReportID: "billing-3", Traffic: map[int][2]int64{10: {3, 4}, 1: {5, 6}}},
			}
			s.deferredAccess = []controlplane.AccessReport{spoolTestAccessReport("billing-2")}
			if name == "deferred only" {
				queue = nil
			} else if name == "billing only" {
				queue[0].Access = nil
				s.deferredAccess = nil
			}
			if err := s.persistReportQueue(queue); err != nil {
				t.Fatal(err)
			}
			path, identity, err := s.spoolLocation()
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			var old legacyReportSpool
			if err := json.Unmarshal(data, &old); err != nil {
				t.Fatal(err)
			}
			if old.Version != 1 || old.Identity != identity || old.Checksum != legacySpoolChecksum(t, old) {
				t.Fatal("a0499b9 reader rejects newly persisted billing cache")
			}
			if len(old.Batches) != len(queue) {
				t.Fatal("legacy reader lost billing batches")
			}
			for i, p := range queue {
				if old.Batches[i].ReportID != p.ReportID || !reflect.DeepEqual(old.Batches[i].Traffic, p.Traffic) {
					t.Fatalf("legacy billing batch %d changed", i)
				}
			}
			restored := durableService(t, s.cfg.Kernel.ConfigDir, s.cfg.Panel.URL)
			if err := restored.loadReportSpool(); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(restored.reportQueue, queue) || !reflect.DeepEqual(restored.deferredAccess, s.deferredAccess) {
				t.Fatal("new reader did not recover billing and access extensions losslessly")
			}
		})
	}
}

func TestSpoolAccessExtensionIntegrity(t *testing.T) {
	s := durableService(t, t.TempDir(), "https://panel.example")
	staged := spoolTestAccessReport("billing-1")
	s.deferredAccess = []controlplane.AccessReport{spoolTestAccessReport("billing-2")}
	if err := s.persistReportQueue([]*controlplane.ReportPayload{{ReportID: "billing-1", Traffic: staged.Totals, Access: &staged}}); err != nil {
		t.Fatal(err)
	}
	path, _, err := s.spoolLocation()
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*reportSpool)
	}{
		{"staged data", func(v *reportSpool) { v.Batches[0].Access.Entries[0].Domain = "changed.example" }},
		{"deferred data", func(v *reportSpool) { v.DeferredAccess[0].Entries[0].Domain = "changed.example" }},
		{"billing linkage", func(v *reportSpool) { v.Batches[0].Access.BillingReportID = "other-billing" }},
		{"remove staged", func(v *reportSpool) { v.Batches[0].Access = nil }},
		{"remove deferred", func(v *reportSpool) { v.DeferredAccess = nil }},
		{"remove all extensions", func(v *reportSpool) { v.Batches[0].Access = nil; v.DeferredAccess = nil }},
		{"missing access checksum", func(v *reportSpool) { v.AccessChecksum = "" }},
		{"wrong access checksum", func(v *reportSpool) { v.AccessChecksum = strings.Repeat("0", 64) }},
		{"identity binding", func(v *reportSpool) { v.Identity = "other-node" }},
		{"billing binding", func(v *reportSpool) { v.Batches[0].Traffic[1] = [2]int64{30, 40} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var v reportSpool
			if err := json.Unmarshal(original, &v); err != nil {
				t.Fatal(err)
			}
			tc.mutate(&v)
			// Keep the legacy checksum valid even for changed accounting data.
			var old legacyReportSpool
			if err := json.Unmarshal(spoolTestJSON(t, v), &old); err != nil {
				t.Fatal(err)
			}
			v.Checksum = legacySpoolChecksum(t, old)
			data := spoolTestJSON(t, v)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			fresh := durableService(t, s.cfg.Kernel.ConfigDir, s.cfg.Panel.URL)
			if err := fresh.loadReportSpool(); err == nil || !strings.Contains(err.Error(), "access checksum") {
				t.Fatalf("extension corruption accepted or wrong validation branch: %v", err)
			}
			if fresh.spoolLoaded || fresh.pendingReport != nil || len(fresh.reportQueue) != 0 || len(fresh.deferredAccess) != 0 {
				t.Fatal("invalid spool partially changed service state")
			}
			preserved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(preserved) != string(data) {
				t.Fatal("invalid spool was modified")
			}
		})
	}
}

func TestSpoolUnpublishedExtensionFormatPreserved(t *testing.T) {
	for _, name := range []string{"staged", "deferred"} {
		t.Run(name, func(t *testing.T) {
			s := durableService(t, t.TempDir(), "https://panel.example")
			path, identity, err := s.spoolLocation()
			if err != nil {
				t.Fatal(err)
			}
			// Reproduce the unpublished whole-object checksum without access_checksum.
			v := struct {
				Version        int                         `json:"version"`
				Identity       string                      `json:"identity"`
				Batches        []spoolBatch                `json:"batches"`
				DeferredAccess []controlplane.AccessReport `json:"deferred_access,omitempty"`
				Checksum       string                      `json:"checksum"`
			}{Version: 1, Identity: identity, Batches: []spoolBatch{{ReportID: "billing-1", Traffic: map[int][2]int64{1: {10, 20}}}}}
			report := spoolTestAccessReport("billing-1")
			if name == "staged" {
				v.Batches[0].Access = &report
			} else {
				v.DeferredAccess = []controlplane.AccessReport{report}
			}
			v.Checksum = fmt.Sprintf("%x", sha256.Sum256(spoolTestJSON(t, v)))
			data := spoolTestJSON(t, v)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := s.loadReportSpool(); err == nil || !strings.Contains(err.Error(), "unpublished extension format") {
				t.Fatalf("unpublished extension format silently accepted: %v", err)
			}
			preserved, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(preserved) != string(data) || s.spoolLoaded || s.pendingReport != nil || len(s.deferredAccess) != 0 {
				t.Fatal("unpublished cache was modified or partially restored")
			}
		})
	}
}
