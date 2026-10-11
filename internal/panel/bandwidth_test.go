package panel

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/bandwidthcheck"
	"github.com/micah123321/mi-node/internal/config"
)

func TestBandwidthClientAuthenticationAndRawAcknowledgement(t *testing.T) {
	for _, machine := range []bool{false, true} {
		name := "legacy"
		if machine {
			name = "machine_zero_node"
		}
		t.Run(name, func(t *testing.T) {
			payload := make([]byte, 262144)
			if _, err := rand.Read(payload); err != nil {
				t.Fatal(err)
			}
			var compressed bytes.Buffer
			gz := gzip.NewWriter(&compressed)
			gz.Write(payload)
			gz.Close()
			if compressed.Len() < len(payload)*9/10 {
				t.Fatal("test payload is compressible")
			}
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				if machine {
					if q.Get("machine_id") != "7" || q.Get("node_id") != "0" || q.Get("token") != "fixture-token" {
						t.Errorf("incorrect machine auth")
					}
				} else if q.Get("node_id") != "2" || q.Get("server_token") != "fixture-token" {
					t.Errorf("incorrect legacy auth")
				}
				if r.Header.Get("Accept-Encoding") != "identity" || r.Header.Get("Content-Encoding") != "" {
					t.Errorf("compression enabled")
				}
				switch r.URL.Path {
				case "/api/v2/server/bandwidth/config":
					if r.Method != "GET" {
						t.Errorf("config method = %s", r.Method)
					}
					json.NewEncoder(w).Encode(map[string]any{"data": bandwidthcheck.Config{Enabled: true, State: bandwidthcheck.State{BlockService: true}, ProbeID: "lease"}})
				case "/api/v2/server/bandwidth/upload":
					if r.Method != "POST" || r.Header.Get("Content-Type") != "application/octet-stream" || q.Get("probe_id") != "lease" || q.Get("warmup") != "1" {
						t.Errorf("incorrect upload contract")
					}
					b, _ := io.ReadAll(r.Body)
					if !bytes.Equal(b, payload) {
						t.Errorf("raw upload changed")
					}
					time.Sleep(30 * time.Millisecond)
					json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"probe_id": "lease", "bytes_received": len(b)}})
				case "/api/v2/server/bandwidth/report":
					var raw map[string]any
					if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
						t.Errorf("report decode: %v", err)
					}
					if len(raw) != 5 || raw["probe_id"] != "lease" || raw["policy_revision"] != "v1" || raw["error"] != nil {
						t.Errorf("report payload differs: %+v", raw)
					}
					json.NewEncoder(w).Encode(map[string]any{"data": bandwidthcheck.State{NextCheckAt: 123}})
				default:
					t.Errorf("unexpected route: %s", r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()
			cfg := config.PanelConfig{URL: ts.URL, Token: "fixture-token", NodeID: 2}
			if machine {
				cfg.MachineID = 7
				cfg.NodeID = 0
			}
			parent := NewClient(cfg)
			client := parent.NewBandwidthClient()
			defer client.Close()
			if client.httpClient == parent.httpClient || client.httpClient.Transport == parent.httpClient.Transport {
				t.Fatal("bandwidth shares heartbeat transport")
			}
			policy, err := client.Config(context.Background())
			if err != nil || !policy.BlockService {
				t.Fatalf("policy=%+v error=%v", policy, err)
			}
			started := time.Now()
			ack, err := client.Upload(context.Background(), "lease", true, payload)
			if err != nil || ack != int64(len(payload)) {
				t.Fatalf("ack=%d error=%v", ack, err)
			}
			if time.Since(started) < 30*time.Millisecond {
				t.Fatal("upload returned before acknowledgement")
			}
			state, err := client.Report(context.Background(), bandwidthcheck.Report{ProbeID: "lease", PolicyRevision: "v1", BytesReceived: 100, DurationMS: 200})
			if err != nil || state.NextCheckAt != 123 {
				t.Fatalf("state=%+v error=%v", state, err)
			}
		})
	}
}

func TestBandwidthClientRejectsUnconfirmedUploads(t *testing.T) {
	for _, response := range []string{
		`{"data":{"probe_id":"wrong","bytes_received":3}}`,
		`{"data":{"probe_id":"lease","bytes_received":2}}`,
		`{"data":null}`, `not-json`,
	} {
		t.Run(response, func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, response) }))
			defer ts.Close()
			client := NewClient(config.PanelConfig{URL: ts.URL}).NewBandwidthClient()
			defer client.Close()
			if _, err := client.Upload(context.Background(), "lease", false, []byte("abc")); err == nil {
				t.Fatal("invalid acknowledgement accepted")
			}
		})
	}
}

func TestBandwidthClientHTTPErrorAndOldPanelAreSanitized(t *testing.T) {
	for _, status := range []int{404, 500} {
		ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); io.WriteString(w, r.URL.RawQuery) }))
		client := NewClient(config.PanelConfig{URL: ts.URL, Token: "must-not-leak"}).NewBandwidthClient()
		_, err := client.Config(context.Background())
		if err == nil || strings.Contains(err.Error(), "must-not-leak") {
			t.Fatalf("unsanitized error: %v", err)
		}
		if status == 404 && !errors.Is(err, bandwidthcheck.ErrUnsupported) {
			t.Fatalf("404 compatibility: %v", err)
		}
		client.Close()
		ts.Close()
	}
}

func TestBandwidthClientRedirectDoesNotForwardCredentials(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { targetCalls.Add(1) }))
	defer target.Close()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
	}))
	defer ts.Close()
	client := NewClient(config.PanelConfig{URL: ts.URL, Token: "must-not-leak"}).NewBandwidthClient()
	defer client.Close()
	_, err := client.Upload(context.Background(), "probe", false, []byte("abc"))
	if err == nil || strings.Contains(err.Error(), "must-not-leak") || targetCalls.Load() != 0 {
		t.Fatalf("redirect followed or leaked: %v calls=%d", err, targetCalls.Load())
	}
}

func TestBandwidthClientCancellation(t *testing.T) {
	entered := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done() }))
	defer ts.Close()
	client := NewClient(config.PanelConfig{URL: ts.URL, Token: "must-not-leak"}).NewBandwidthClient()
	defer client.Close()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := client.Config(ctx); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("request not started")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || strings.Contains(err.Error(), "must-not-leak") {
			t.Fatalf("cancellation error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("upload context cancellation ignored")
	}
}

func TestBandwidthClientMissingStateDoesNotClearBlock(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `{"data":{}}`) }))
	defer ts.Close()
	client := NewClient(config.PanelConfig{URL: ts.URL}).NewBandwidthClient()
	defer client.Close()
	if _, err := client.Config(context.Background()); err == nil {
		t.Fatal("empty config accepted")
	}
	if _, err := client.Report(context.Background(), bandwidthcheck.Report{}); err == nil {
		t.Fatal("empty report state accepted")
	}
}
