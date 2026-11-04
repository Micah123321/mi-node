package machine

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/panel"
)

func TestCleanupStaleNodeDataKeepsCurrentAndSharedData(t *testing.T) {
	base := t.TempDir()
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: base}}
	o := &Orchestrator{cfg: cfg}

	for _, name := range []string{"node-11", "node-22", "node-invalid"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(base, name, "state"), []byte(name), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	shared := filepath.Join(base, "shared-cache")
	if err := os.WriteFile(shared, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	o.cleanupStaleNodeData([]panel.MachineNode{{ID: 22}})

	if _, err := os.Stat(filepath.Join(base, "node-11")); !os.IsNotExist(err) {
		t.Fatalf("deleted node directory still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "node-22", "state")); err != nil {
		t.Fatalf("current node directory was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "node-invalid")); err != nil {
		t.Fatalf("unrelated directory was removed: %v", err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("shared machine data was removed: %v", err)
	}

	o.removeNodeData(22)
	if _, err := os.Stat(filepath.Join(base, "node-22")); !os.IsNotExist(err) {
		t.Fatalf("explicitly removed node directory still exists: %v", err)
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("shared machine data changed after node removal: %v", err)
	}
}

func TestRediscoverDeletesRemovedPanelNodeData(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/machine/nodes" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(panel.MachineNodesResponse{})
	}))
	defer ts.Close()

	base := t.TempDir()
	cfg := &config.Config{
		Panel:   config.PanelConfig{URL: ts.URL},
		Machine: &config.MachineConfig{MachineID: 7, Token: "fixture"},
		Kernel:  config.KernelConfig{ConfigDir: base},
	}
	o := New(cfg)
	dir := cfg.MachineNodeConfigDir(11)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	close(done)
	o.nodes[11] = &nodeHandle{cancel: func() {}, done: done}

	o.rediscover(context.Background())

	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("rediscovery did not remove deleted node data: %v", err)
	}
	if _, exists := o.nodes[11]; exists {
		t.Fatal("removed panel node remained registered")
	}
}

func TestCleanupStaleNodeDataIgnoresMissingBase(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{ConfigDir: filepath.Join(t.TempDir(), "missing")}}
	(&Orchestrator{cfg: cfg}).cleanupStaleNodeData(nil)
}
