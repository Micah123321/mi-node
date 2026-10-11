package machine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/bandwidthcheck"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/panel"
)

func TestEmptyMachineStillUploadsAndRediscoveryUsesPanelDecision(t *testing.T) {
	var discoveries, warmups, uploads, wireBytes atomic.Int32
	reports := make(chan bandwidthcheck.Report, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v2/server/machine/nodes":
			discoveries.Add(1)
			json.NewEncoder(w).Encode(panel.MachineNodesResponse{Nodes: []panel.MachineNode{}})
		case "/api/v2/server/handshake":
			json.NewEncoder(w).Encode(panel.HandshakeResponse{})
		case "/api/v2/server/bandwidth/config":
			if r.URL.Query().Get("machine_id") != "7" || r.URL.Query().Get("node_id") != "0" || r.URL.Query().Get("token") != "fixture" {
				t.Errorf("invalid machine scope")
			}
			json.NewEncoder(w).Encode(map[string]any{"data": bandwidthcheck.Config{Enabled: true, ProbeID: "lease", PolicyRevision: "v1", ThresholdMbps: 8, SampleSeconds: 8, ChunkBytes: 32768, MaxBytes: 65536, TimeoutSeconds: 2}})
		case "/api/v2/server/bandwidth/upload":
			b, _ := io.ReadAll(r.Body)
			wireBytes.Add(int32(len(b)))
			if r.URL.Query().Get("warmup") == "1" {
				warmups.Add(1)
			} else {
				uploads.Add(1)
				// Slow acknowledgement must be included, even though Write completed.
				time.Sleep(180 * time.Millisecond)
			}
			json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"probe_id": "lease", "bytes_received": len(b)}})
		case "/api/v2/server/bandwidth/report":
			var report bandwidthcheck.Report
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Errorf("decode: %v", err)
			}
			reports <- report
			// The backend fixture starts with two low samples; it owns the transition.
			low := report.Error == nil && report.DurationMS > 0 && float64(report.BytesReceived)*8/(report.DurationMS*1000) < 8
			json.NewEncoder(w).Encode(map[string]any{"data": bandwidthcheck.State{Suspended: low, NextCheckAt: 123}})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()
	cfg := &config.Config{Panel: config.PanelConfig{URL: ts.URL}, Machine: &config.MachineConfig{MachineID: 7, Token: "fixture"}}
	o := New(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 35*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- o.Run(ctx) }()
	var result bandwidthcheck.Report
	select {
	case result = <-reports:
	case err := <-done:
		t.Fatalf("empty machine stopped before probing: %v", err)
	case <-ctx.Done():
		t.Fatal("empty machine never ran bandwidth probe")
	}
	if result.Error != nil || result.BytesReceived != 32768 || result.DurationMS < 180 || result.ProbeID != "lease" || result.PolicyRevision != "v1" {
		t.Fatalf("report: %+v", result)
	}
	if warmups.Load() != 1 || uploads.Load() != 1 || wireBytes.Load() != 65536 {
		t.Fatalf("wire budget: warmups=%d uploads=%d bytes=%d", warmups.Load(), uploads.Load(), wireBytes.Load())
	}
	deadline := time.Now().Add(time.Second)
	for discoveries.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if discoveries.Load() < 3 {
		t.Fatalf("authoritative transition did not trigger rediscovery: %d", discoveries.Load())
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("runner did not stop with orchestrator")
	}
}
