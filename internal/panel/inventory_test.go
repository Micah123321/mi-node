package panel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/discovery"
)

// Tests using the process initializer are deliberately non-parallel.
func resetInventory(t *testing.T) {
	t.Helper()
	inventoryOnce = sync.Once{}
	inventoryError = nil
	processInventory.Store(nil)
	t.Cleanup(func() {
		inventoryOnce = sync.Once{}
		inventoryError = nil
		processInventory.Store(nil)
	})
}

func TestInventoryEveryBindingAndRetry(t *testing.T) {
	resetInventory(t)
	dir := t.TempDir()
	if err := InitUpdateInventory("build-one", dir); err != nil {
		t.Fatal(err)
	}
	want, err := discovery.Load(dir, "build-one")
	if err != nil {
		t.Fatal(err)
	}
	if err := InitUpdateInventory("changed", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "https"}[secure], func(t *testing.T) {
			var bodies []map[string]json.RawMessage
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v2/server/report" || r.Method != http.MethodPost {
					t.Errorf("request: %s %s", r.Method, r.URL.Path)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				bodies = append(bodies, body)
				var got discovery.Inventory
				if err := json.Unmarshal(body["update_inventory"], &got); err != nil || got != want {
					t.Errorf("inventory: %+v %v", got, err)
				}
				if string(body["token"]) != "\"original-token\"" || string(body["report_id"]) != "\"batch-unchanged\"" || string(body["traffic"]) != "{\"7\":[12,34]}" {
					t.Errorf("accounting/auth altered: %s", body)
				}
				if len(bodies)%2 == 1 {
					_, _ = w.Write([]byte(`{"data":false}`))
				} else {
					_, _ = w.Write([]byte(`{"data":true}`))
				}
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			legacy := NewClient(config.PanelConfig{URL: server.URL, Token: "original-token", NodeID: 11, NodeType: "vless"})
			machine := NewClient(config.PanelConfig{URL: server.URL, Token: "original-token", MachineID: 22})
			legacy.httpClient, machine.httpClient = server.Client(), server.Client()
			for _, c := range []*Client{legacy, machine, machine.ForNode(33), machine.ForNode(44)} {
				for attempt := range 2 {
					err := c.Report(context.Background(), "batch-unchanged", map[int][2]int64{7: {12, 34}}, nil, nil, 1, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil)
					if (err == nil) != (attempt == 1) {
						t.Fatalf("ACK attempt %d: %v", attempt, err)
					}
				}
				a, b := bodies[len(bodies)-2], bodies[len(bodies)-1]
				if !reflect.DeepEqual(a, b) {
					t.Fatal("retry body changed")
				}
				if c.machineID > 0 && string(b["machine_id"]) != "22" {
					t.Fatal("machine binding changed")
				}
				var node int
				_ = json.Unmarshal(b["node_id"], &node)
				if node != c.nodeID {
					t.Fatalf("node binding: %d != %d", node, c.nodeID)
				}
			}
		})
	}
}

func TestInventoryFailureDoesNotBlockReport(t *testing.T) {
	resetInventory(t)
	path := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(path, []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InitUpdateInventory("dev", filepath.Join(path, "child")); err == nil {
		t.Fatal("expected initialization error")
	}
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if _, ok := body["update_inventory"]; ok {
			t.Error("failed discovery leaked inventory")
		}
		_, _ = w.Write([]byte(`{"data":true}`))
	})
	defer server.Close()
	if err := client.Report(context.Background(), "existing", nil, nil, nil, 0, [2]uint64{}, [2]uint64{}, [2]uint64{}, nil); err != nil {
		t.Fatal(err)
	}
}

func TestHandshakeDiscoveryNonblockingOnce(t *testing.T) {
	resetInventory(t)
	if err := InitUpdateInventory("dev", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	var reports atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	server, client := newTestServer(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v2/server/handshake" {
			if fail.Load() {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			_, _ = w.Write([]byte(`{"settings":[]}`))
			return
		}
		reports.Add(1)
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		for _, field := range []string{"report_id", "traffic", "alive", "online"} {
			if _, ok := body[field]; ok {
				t.Errorf("startup report contains %s", field)
			}
		}
		var inventory discovery.Inventory
		_ = json.Unmarshal(body["update_inventory"], &inventory)
		if inventory.Version != "dev" || inventory.OS != runtime.GOOS {
			t.Errorf("startup inventory: %+v", inventory)
		}
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte(`{"data":true}`))
	})
	defer server.Close()
	defer close(release)
	if _, err := client.Handshake(); err == nil {
		t.Fatal("failed handshake succeeded")
	}
	if reports.Load() != 0 {
		t.Fatal("reported before successful handshake")
	}
	fail.Store(false)
	done := make(chan error, 1)
	go func() { _, err := client.Handshake(); done <- err }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("handshake blocked on report")
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("startup report missing")
	}
	if _, err := client.Handshake(); err != nil {
		t.Fatal(err)
	}
	if reports.Load() != 1 {
		t.Fatalf("startup report count: %d", reports.Load())
	}
}
