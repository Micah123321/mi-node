package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRemoveNodeBindingPreservesRemainingInstances(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	source := "instances:\n  - panel:\n      url: http://panel.example/\n      token: token173\n      node_id: 173\n    kernel:\n      config_dir: /var/lib/mi-node/node-173\n  - panel:\n      url: http://panel.example\n      token: token452\n      node_id: 452\n    kernel:\n      config_dir: /var/lib/mi-node/node-452\nkernel:\n  type: singbox\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, err := RemoveNodeBinding(path, "http://panel.example", 173)
	if err != nil || !removed {
		t.Fatalf("RemoveNodeBinding: removed=%v error=%v", removed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if strings.Contains(text, "token173") || strings.Contains(text, "node-173") {
		t.Fatalf("deleted binding remained in config:\n%s", text)
	}
	if !strings.Contains(text, "token452") || !strings.Contains(text, "node-452") {
		t.Fatalf("remaining binding was lost:\n%s", text)
	}
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Instances) != 1 || root.Instances[0].Panel.NodeID != 452 {
		t.Fatalf("loaded instances: %+v", root.Instances)
	}
}

func TestRemoveNodeBindingLegacyRemovesPanelOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	source := "panel:\n  url: http://panel.example\n  token: fixture\n  node_id: 173\nkernel:\n  type: singbox\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveNodeBinding(path, "http://panel.example/", 173)
	if err != nil || !removed {
		t.Fatalf("RemoveNodeBinding: removed=%v error=%v", removed, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "panel:") {
		t.Fatalf("legacy panel binding remained: %s", data)
	}
}
func TestRemoveNodeBindingPreservesSiblingNodes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	source := "instances:\n  - panel:\n      url: http://panel.example\n      token: token\n    nodes:\n      - node_id: 173\n        node_type: tuic\n      - node_id: 452\n        node_type: mieru\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	removed, err := RemoveNodeBinding(path, "http://panel.example/", 173)
	if err != nil || !removed {
		t.Fatalf("RemoveNodeBinding: removed=%v error=%v", removed, err)
	}
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Instances) != 1 || len(root.Instances[0].Nodes) != 1 || root.Instances[0].Nodes[0].NodeID != 452 {
		t.Fatalf("remaining nodes: %+v", root.Instances)
	}
}

func TestRemoveNodeBindingHandlesConcurrentDeletesAndEmptyConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	var source strings.Builder
	source.WriteString("instances:\n")
	for _, id := range []int{173, 452, 453} {
		fmt.Fprintf(&source, "  - panel: {url: http://panel.example, token: fixture, node_id: %d}\n", id)
	}
	if err := os.WriteFile(path, []byte(source.String()), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for _, id := range []int{173, 452, 453} {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if _, err := RemoveNodeBinding(path, "http://panel.example", id); err != nil {
				t.Error(err)
			}
		}(id)
	}
	wg.Wait()
	root, err := LoadRoot(path)
	if err != nil {
		t.Fatal(err)
	}
	instances, err := root.NormalizeInstances()
	if err != nil || len(instances) != 0 {
		t.Fatalf("instances=%v err=%v", instances, err)
	}
}

func TestRemoveNodeBindingRetainsSiblingPathsAndRootNodes(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint(nested), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yml")
			source := "panel: {url: http://panel.example, token: fixture, node_id: 173}\nnodes: [{node_id: 173}, {node_id: 452}]\n"
			if nested {
				source = "instances:\n  - panel: {url: http://panel.example, token: fixture, node_id: 173}\n    nodes: [{node_id: 173}, {node_id: 452}]\n"
			}
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			before, err := LoadRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			beforeInst, _ := before.NormalizeInstances()
			expected := beforeInst[0].ExpandNodes()[1]
			if _, err := RemoveNodeBinding(path, "http://panel.example", 173); err != nil {
				t.Fatal(err)
			}
			after, err := LoadRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			afterInst, _ := after.NormalizeInstances()
			actual := afterInst[0].ExpandNodes()[0]
			if actual.Panel.NodeID != 452 || actual.Kernel.ConfigDir != expected.Kernel.ConfigDir || actual.Cert.CertDir != expected.Cert.CertDir {
				t.Fatalf("sibling paths changed: before=%+v after=%+v", expected, actual)
			}
		})
	}
}

func TestRemoveNodeBindingIgnoresChangedCredentialsAndOtherPanel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	source := "instances:\n  - panel: {url: http://one.example, token: fixture, node_id: 173}\n  - panel: {url: http://two.example, token: fixture, node_id: 173}\n"
	if err := os.WriteFile(path, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	root, _ := LoadRoot(path)
	nodes, _ := root.NormalizeInstances()
	expected := *nodes[0]
	expected.Panel.Token = "old-token"
	if removed, err := RemoveNodeBindingIfUnchanged(path, &expected); err != nil || removed {
		t.Fatalf("stale event removed binding: %v %v", removed, err)
	}
	if _, err := RemoveNodeBindingIfUnchanged(path, nodes[0]); err != nil {
		t.Fatal(err)
	}
	after, _ := LoadRoot(path)
	remaining, _ := after.NormalizeInstances()
	if len(remaining) != 1 || remaining[0].Panel.URL != "http://two.example" {
		t.Fatalf("other panel affected")
	}
}

func TestAtomicBindingWritePreservesConcurrentFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	original := []byte("instances: []\n")
	changed := []byte("instances: []\n# operator change\n")
	if err := os.WriteFile(path, changed, 0600); err != nil {
		t.Fatal(err)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(original, &doc); err != nil {
		t.Fatal(err)
	}
	if err := writeYAMLAtomically(path, original, &doc); err == nil {
		t.Fatal("concurrent write not detected")
	}
	data, _ := os.ReadFile(path)
	if string(data) != string(changed) {
		t.Fatal("operator config lost")
	}
}
