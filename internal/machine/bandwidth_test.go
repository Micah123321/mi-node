package machine

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/bandwidthcheck"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/panel"
)

type machineDownloadTransport func(*http.Request) (*http.Response, error)

func (f machineDownloadTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEmptyMachineStillDownloadsAndRediscoveryUsesPanelDecision(t *testing.T) {
	var discoveries, downloads, uploads atomic.Int32
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
			json.NewEncoder(w).Encode(map[string]any{"data": bandwidthcheck.Config{Enabled: true,
				State: bandwidthcheck.State{Suspended: true}, ProbeID: "lease", PolicyRevision: "cf-v1",
				ProbeMethod: bandwidthcheck.CloudflareDownloadV1, DownloadURL: bandwidthcheck.CloudflareDownloadURL,
				ThresholdMbps: 8, SampleSeconds: 8, ChunkBytes: 262144, MaxBytes: 10000000, TimeoutSeconds: 120}})
		case "/api/v2/server/bandwidth/upload":
			uploads.Add(1)
			t.Error("Cloudflare probe uploaded sample bytes to the panel")
			http.Error(w, "unexpected upload", http.StatusBadRequest)
		case "/api/v2/server/bandwidth/report":
			var report bandwidthcheck.Report
			if err := json.NewDecoder(r.Body).Decode(&report); err != nil {
				t.Errorf("decode: %v", err)
			}
			reports <- report
			// Only the panel owns recovery and the resulting rediscovery transition.
			json.NewEncoder(w).Encode(map[string]any{"data": bandwidthcheck.State{NextCheckAt: 123}})
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
	ctx = bandwidthcheck.WithDownloadTransportForTesting(ctx, machineDownloadTransport(func(r *http.Request) (*http.Response, error) {
		downloads.Add(1)
		if r.Method != http.MethodGet || r.URL.String() != bandwidthcheck.CloudflareDownloadURL ||
			r.Header.Get("Authorization") != "" || r.Header.Get("Cookie") != "" || len(r.URL.Query()) != 1 {
			t.Errorf("download target or authentication isolation changed")
		}
		return &http.Response{StatusCode: http.StatusOK, ContentLength: 10000000,
			Header: http.Header{"Content-Type": {"application/octet-stream"}},
			Body:   io.NopCloser(strings.NewReader(strings.Repeat("x", 10000000)))}, nil
	}))
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
	if result.Error != nil || result.BytesReceived != 10000000-262144 || result.DurationMS <= 0 ||
		result.ProbeID != "lease" || result.PolicyRevision != "cf-v1" || result.ProbeMethod != bandwidthcheck.CloudflareDownloadV1 {
		t.Fatalf("report: %+v", result)
	}
	if mbps := float64(result.BytesReceived) * 8 / (result.DurationMS * 1000); mbps < 8 {
		t.Fatalf("fast download was reported low: %.3f Mbps", mbps)
	}
	if downloads.Load() != 1 || uploads.Load() != 0 {
		t.Fatalf("unexpected sample requests: downloads=%d uploads=%d", downloads.Load(), uploads.Load())
	}
	deadline := time.Now().Add(time.Second)
	for discoveries.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if discoveries.Load() < 3 {
		t.Fatalf("authoritative recovery did not trigger rediscovery: %d", discoveries.Load())
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
