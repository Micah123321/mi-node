package controlplane

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
)

func TestPanelResyncFetchesFullSnapshotAfterNotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", "\"fixture-etag\"")
		switch r.URL.Path {
		case "/api/v1/server/UniProxy/config":
			w.Write([]byte("{\"protocol\":\"shadowsocks\",\"server_port\":8388}"))
		case "/api/v1/server/UniProxy/user":
			w.Write([]byte("{\"users\":[{\"id\":1,\"uuid\":\"fixture-user\"}]}"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	cp := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "fixture-token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	ctx := context.Background()
	if snapshot, err := cp.Poll(ctx); err != nil || snapshot.Config == nil || snapshot.Users == nil {
		t.Fatalf("initial poll: snapshot=%+v error=%v", snapshot, err)
	}
	if snapshot, err := cp.Poll(ctx); err != nil || snapshot.Config != nil || snapshot.Users != nil {
		t.Fatalf("conditional poll: snapshot=%+v error=%v", snapshot, err)
	}
	cp.RequestResync()
	if snapshot, err := cp.Poll(ctx); err != nil || snapshot.Config == nil || snapshot.Users == nil {
		t.Fatalf("full resync: snapshot=%+v error=%v", snapshot, err)
	}
}

func TestPanelPollRetriesCompleteSnapshotAfterPartialFailure(t *testing.T) {
	var failUsers atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/server/UniProxy/user" && failUsers.Swap(false) {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.Header.Get("If-None-Match") != "" {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", "\"fixture-etag\"")
		if r.URL.Path == "/api/v1/server/UniProxy/config" {
			w.Write([]byte("{\"protocol\":\"shadowsocks\",\"server_port\":8388}"))
		} else {
			w.Write([]byte("{\"users\":[{\"id\":1,\"uuid\":\"fixture-user\"}]}"))
		}
	}))
	defer server.Close()
	cp := NewPanelControlPlane(config.PanelConfig{URL: server.URL, Token: "fixture-token", NodeID: 1}, config.WSConfig{}, config.KernelConfig{Type: "singbox"})
	ctx := context.Background()
	if _, err := cp.Poll(ctx); err != nil {
		t.Fatal(err)
	}
	cp.RequestResync()
	failUsers.Store(true)
	if _, err := cp.Poll(ctx); err == nil {
		t.Fatal("expected partial snapshot fetch to fail")
	}
	if snapshot, err := cp.Poll(ctx); err != nil || snapshot.Config == nil || snapshot.Users == nil {
		t.Fatalf("retry lost part of the snapshot: snapshot=%+v error=%v", snapshot, err)
	}
}
