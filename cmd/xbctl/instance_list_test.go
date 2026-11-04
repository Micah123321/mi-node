package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestInstanceListUsesCurrentConfigAfterAutomaticRemoval(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yml")
	meta := filepath.Join(dir, "install-meta.json")
	// The shell has no credential env loaded; displaying bindings still works.
	t.Setenv("MI_LIST_TEST_TOKEN", "")
	if err := os.WriteFile(cfg, []byte("instances: [{panel: {url: http://panel.example, node_id: 452, token_env: MI_LIST_TEST_TOKEN}}]\n"), 0600); err != nil {
		t.Fatal(err)
	}
	stale := `{"instances":[{"id":"old-173","mode":"node","panel_url":"http://panel.example","node_id":173},{"id":"old-452","mode":"node","panel_url":"http://panel.example","node_id":452}]}`
	if err := os.WriteFile(meta, []byte(stale), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := collectInstanceRowsAt(cfg, meta, "active", "disabled")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Target != "node_id=452" {
		t.Fatalf("stale rows survived: %+v", rows)
	}
	if err := os.WriteFile(cfg, []byte("instances: []\n"), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err = collectInstanceRowsAt(cfg, meta, "active", "disabled")
	if err != nil || len(rows) != 0 {
		t.Fatalf("empty config resurrected stale rows: %v %v", rows, err)
	}
	if err := os.WriteFile(cfg, []byte("instances: [\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := collectInstanceRowsAt(cfg, meta, "active", "disabled"); err == nil {
		t.Fatal("invalid config silently used stale rows")
	}
}

func TestInstanceListUsesInstallerSummaryOnlyWithoutConfig(t *testing.T) {
	dir := t.TempDir()
	meta := filepath.Join(dir, "install-meta.json")
	if err := os.WriteFile(meta, []byte(`{"instances":[{"id":"node-452","mode":"node","panel_url":"http://panel.example","node_id":452}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	rows, err := collectInstanceRowsAt(filepath.Join(dir, "missing.yml"), meta, "active", "disabled")
	if err != nil || len(rows) != 1 || rows[0].Target != "node_id=452" {
		t.Fatalf("installer fallback: %v %v", rows, err)
	}
}
