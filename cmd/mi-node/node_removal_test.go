package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/config"
	"gopkg.in/yaml.v3"
)

func removalFixture(t *testing.T) (string, []*config.Config) {
	t.Helper()
	base := t.TempDir()
	path := filepath.Join(base, "config.yml")
	root := &config.RootConfig{}
	for _, id := range []int{173, 452} {
		cfg := config.Config{Panel: config.PanelConfig{URL: "http://panel.example", NodeID: id, Token: "fixture"}, Kernel: config.KernelConfig{Type: "singbox"}}
		name, err := cfg.AutoInstanceID()
		if err != nil {
			t.Fatal(err)
		}
		cfg.Kernel.ConfigDir = filepath.Join(base, "instances", name)
		if err := os.MkdirAll(cfg.Kernel.ConfigDir, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cfg.Kernel.ConfigDir, "state"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
		root.Instances = append(root.Instances, cfg)
	}
	data, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	loaded, err := config.LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	nodes, err := loaded.NormalizeInstances()
	if err != nil {
		t.Fatal(err)
	}
	return path, nodes
}

func TestDeletedStaticBindingRemovesOnlyItsOwnDirectory(t *testing.T) {
	path, nodes := removalFixture(t)
	if err := removeDeletedNodeBinding(path, nodes[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nodes[0].Kernel.ConfigDir); !os.IsNotExist(err) {
		t.Fatalf("deleted directory retained: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nodes[1].Kernel.ConfigDir, "state")); err != nil {
		t.Fatal("sibling directory lost", err)
	}
	root, err := config.LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := root.NormalizeInstances()
	if len(actual) != 1 || actual[0].Panel.NodeID != 452 {
		t.Fatalf("incorrect remaining binding: %v", actual)
	}
	if err := removeDeletedNodeBinding(path, nodes[1]); err != nil {
		t.Fatal(err)
	}
	root, err = config.LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ = root.NormalizeInstances()
	if len(actual) != 0 {
		t.Fatal("last node resurrected")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if next, err := waitForNodeBindings(ctx, path); err != nil || next != nil {
		t.Fatalf("idle exit: %v %v", next, err)
	}
}

func TestNodeCleanupKeepsSharedCustomAndChangedBinding(t *testing.T) {
	path, nodes := removalFixture(t)
	shared := *nodes[1]
	shared.Kernel.ConfigDir = nodes[0].Kernel.ConfigDir
	if canRemoveNodeDir(path, nodes[0], []*config.Config{&shared}) {
		t.Fatal("shared dir considered disposable")
	}
	parent := *nodes[0]
	parent.Kernel.ConfigDir = filepath.Dir(path)
	if canRemoveNodeDir(path, &parent, nil) {
		t.Fatal("config parent considered disposable")
	}
	custom := *nodes[0]
	custom.Kernel.ConfigDir = t.TempDir()
	if canRemoveNodeDir(path, &custom, nil) {
		t.Fatal("custom dir considered disposable")
	}
	stale := *nodes[0]
	stale.Panel.Token = "stale-token"
	if err := removeDeletedNodeBinding(path, &stale); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(nodes[0].Kernel.ConfigDir); err != nil {
		t.Fatal("stale deletion touched directory")
	}
}

func TestEmptyConfigCanAcceptNewBindings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(path, []byte("instances: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan *config.RootConfig, 1)
	go func() {
		root, err := waitForNodeBindings(ctx, path)
		if err != nil {
			t.Error(err)
		}
		result <- root
	}()
	// Repeated atomic save is unnecessary: the idle watcher also reads after subscribing.
	if err := os.WriteFile(path, []byte("instances: [{panel: {url: http://panel.example, token: fixture, node_id: 452}}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	select {
	case root := <-result:
		if root == nil {
			t.Fatal("new binding not observed")
		}
	case <-ctx.Done():
		t.Fatal("idle watcher missed new bindings")
	}
}
