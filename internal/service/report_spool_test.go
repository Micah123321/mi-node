package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

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
