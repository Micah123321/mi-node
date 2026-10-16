package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
	"gopkg.in/yaml.v3"
)

func TestParseRemoveArgs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		id    int
		panel string
	}{
		{"node ID", []string{"458"}, 458, ""},
		{"panel selector", []string{"458", "--panel", " https://panel.example.com "}, 458, "https://panel.example.com"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, panel, err := parseRemoveArgs(tc.args)
			if err != nil || id != tc.id || panel != tc.panel {
				t.Fatalf("parseRemoveArgs = %d %q %v", id, panel, err)
			}
		})
	}
}

func TestRemoveRejectsInvalidIDsBeforeReadingConfig(t *testing.T) {
	for _, args := range [][]string{nil, {"0"}, {"-458"}, {"458abc"}, {"458.0"}, {"999999999999999999999"}, {"458", "461"}, {"458", "--panel"}, {"458", "--panel", " "}, {"458", "--unknown", "value"}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			err := run(append([]string{"remove"}, args...))
			if err == nil || (!strings.Contains(err.Error(), "invalid node-id") && !strings.Contains(err.Error(), "usage: xbctl remove")) {
				t.Fatalf("invalid remove args reached config or deletion: %v", err)
			}
		})
	}
}

func TestFindRemoveNodeUsesPanelNodeID(t *testing.T) {
	panel := "http://panel.example.com"
	instances := []config.Config{
		{InstanceID: "node-137", Panel: config.PanelConfig{URL: panel, NodeID: 137}},
		{InstanceID: "long-binding-node-458", Panel: config.PanelConfig{URL: panel, NodeID: 458}},
		{InstanceID: "node-458-in-name-only", Panel: config.PanelConfig{URL: panel, NodeID: 461}},
		{InstanceID: "machine-458", Panel: config.PanelConfig{URL: panel, NodeID: 458}, Machine: &config.MachineConfig{MachineID: 458}},
		{InstanceID: "standalone", Panel: config.PanelConfig{URL: panel, NodeID: 458}, Standalone: &config.StandaloneConfig{Enabled: true}},
		{InstanceID: "multi-node", Panel: config.PanelConfig{URL: panel, NodeID: 458}, Nodes: []config.NodeEntry{{NodeID: 458}, {NodeID: 461}}},
	}
	target, err := findRemoveNode(instances, 458, "")
	if err != nil || instances[target].InstanceID != "long-binding-node-458" {
		t.Fatalf("selected wrong binding: %s %v", instances[target].InstanceID, err)
	}
	if _, err := findRemoveNode(instances, 459, ""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing node: %v", err)
	}
}

func TestFindRemoveNodeRequiresUnambiguousBinding(t *testing.T) {
	instances := []config.Config{
		{InstanceID: "panel-one-458", Panel: config.PanelConfig{URL: "http://one.example.com", NodeID: 458}},
		{InstanceID: "panel-two-458", Panel: config.PanelConfig{URL: "http://two.example.com", NodeID: 458}},
	}
	if _, err := findRemoveNode(instances, 458, ""); err == nil || !strings.Contains(err.Error(), "multiple bindings") {
		t.Fatalf("ambiguous node selected: %v", err)
	}
	target, err := findRemoveNode(instances, 458, "http://two.example.com")
	if err != nil || instances[target].InstanceID != "panel-two-458" {
		t.Fatalf("panel selection: %s %v", instances[target].InstanceID, err)
	}
	instances = append(instances, config.Config{InstanceID: "panel-two-duplicate", Panel: config.PanelConfig{URL: "http://two.example.com", NodeID: 458}})
	if _, err := findRemoveNode(instances, 458, "http://two.example.com"); err == nil {
		t.Fatal("duplicate binding selected")
	}
}

func TestRemoveNodeBindingPreservesCollidingInstance(t *testing.T) {
	for _, targetFirst := range []bool{true, false} {
		name := "target last"
		if targetFirst {
			name = "target first"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			configPath := filepath.Join(dir, "config.yml")
			credentialsPath := filepath.Join(dir, "credentials.env")
			metaPath := filepath.Join(dir, "install-meta.json")
			target := config.Config{InstanceID: "custom-target", Panel: config.PanelConfig{
				URL: "https://panel.example.com", NodeID: 458, TokenEnv: "TARGET_TOKEN",
			}}
			collisionID, err := target.AutoInstanceID()
			if err != nil {
				t.Fatal(err)
			}
			other := config.Config{
				InstanceID: collisionID,
				Panel:      config.PanelConfig{URL: target.Panel.URL, NodeID: 461, TokenEnv: "OTHER_TOKEN"},
				HealthPort: 65531,
				Kernel:     config.KernelConfig{Type: "xray", ConfigDir: "/fixture/other"},
			}
			root := &config.RootConfig{
				Config:    config.Config{Log: config.LogConfig{Level: "debug", Output: "stdout"}},
				Instances: []config.Config{other, target},
			}
			if targetFirst {
				root.Instances = []config.Config{target, other}
			}
			if err := writeRootConfig(configPath, root); err != nil {
				t.Fatal(err)
			}
			if err := writeInstallMetaVersioned(metaPath, root, "fixture-version", ""); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(credentialsPath, []byte("TARGET_TOKEN=target-fixture\nOTHER_TOKEN=other-fixture\nDNS_TOKEN=dns-fixture\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			want, err := loadWritableRootConfig(configPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, inst := range want.Instances {
				if inst.Panel.NodeID == other.Panel.NodeID {
					want.Instances = []config.Config{inst}
					break
				}
			}
			otherID, err := other.AutoInstanceID()
			if err != nil {
				t.Fatal(err)
			}
			var actions []string
			service := func(action string, extra ...string) error {
				actions = append(actions, action)
				if len(extra) != 0 {
					t.Fatalf("unexpected service arguments: %v", extra)
				}
				got, err := loadWritableRootConfig(configPath)
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("persisted config changed surviving instance or shared settings: got=%+v want=%+v err=%v", got, want, err)
				}
				credentials, err := os.ReadFile(credentialsPath)
				if err != nil || string(credentials) != "OTHER_TOKEN=other-fixture\nDNS_TOKEN=dns-fixture\n" {
					t.Fatalf("surviving credentials changed: %q err=%v", credentials, err)
				}
				meta, err := loadInstallMeta(metaPath)
				if err != nil {
					t.Fatal(err)
				}
				if meta.Version != "fixture-version" || meta.ConfigMode != "instances" || meta.InstanceCount != 1 || len(meta.Instances) != 1 || meta.LatestInstanceID != otherID {
					t.Fatalf("incorrect surviving metadata: %+v", meta)
				}
				item := meta.Instances[0]
				if item.ID != otherID || item.PanelURL != other.Panel.URL || item.Mode != "node" || item.NodeID == nil || *item.NodeID != 461 || item.HealthPort != other.HealthPort {
					t.Fatalf("surviving instance metadata changed: %+v", item)
				}
				return nil
			}
			if err := removeNodeBinding(configPath, credentialsPath, metaPath, 458, "", service); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(actions, []string{"restart"}) {
				t.Fatalf("service calls = %v, want [restart]", actions)
			}
		})
	}
}

func TestRemoveNodeBindingPersistsEmptyLegacyConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yml")
	credentialsPath := filepath.Join(dir, "credentials.env")
	metaPath := filepath.Join(dir, "install-meta.json")
	legacy := config.Config{
		Panel:   config.PanelConfig{URL: "https://panel.example.com", NodeID: 458, TokenEnv: "TARGET_TOKEN"},
		Log:     config.LogConfig{Level: "debug", Output: "stdout"},
		Kernel:  config.KernelConfig{Type: "xray"},
		Runtime: config.RuntimeConfig{GoMemLimit: "128MiB", GoGCPercent: 50},
	}
	data, err := yaml.Marshal(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialsPath, []byte("TARGET_TOKEN=target-fixture\nDNS_TOKEN=dns-fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := loadWritableRootConfig(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.Instances) != 0 || len(normalizeRootInstances(root)) != 1 {
		t.Fatal("fixture is not a legacy root binding")
	}
	if err := writeInstallMetaVersioned(metaPath, root, "fixture-version", ""); err != nil {
		t.Fatal(err)
	}
	var actions []string
	service := func(action string, extra ...string) error {
		actions = append(actions, action)
		if len(extra) != 0 {
			t.Fatalf("unexpected service arguments: %v", extra)
		}
		got, err := loadWritableRootConfig(configPath)
		if err != nil {
			t.Fatal(err)
		}
		if len(normalizeRootInstances(got)) != 0 || got.Config.Panel != (config.PanelConfig{}) {
			t.Fatalf("legacy binding restored in persisted config: %+v", got)
		}
		if !reflect.DeepEqual(got.Config.Kernel, legacy.Kernel) || got.Config.Log != legacy.Log || got.Config.Runtime != legacy.Runtime {
			t.Fatalf("shared config settings changed: %+v", got.Config)
		}
		credentials, err := os.ReadFile(credentialsPath)
		if err != nil || string(credentials) != "DNS_TOKEN=dns-fixture\n" {
			t.Fatalf("incorrect credentials after last removal: %q err=%v", credentials, err)
		}
		meta, err := loadInstallMeta(metaPath)
		if err != nil {
			t.Fatal(err)
		}
		if meta.Version != "fixture-version" || meta.ConfigMode != "instances" || meta.InstanceCount != 0 || len(meta.Instances) != 0 || meta.LatestInstanceID != "" {
			t.Fatalf("legacy binding restored in metadata: %+v", meta)
		}
		return nil
	}
	if err := removeNodeBinding(configPath, credentialsPath, metaPath, 458, "https://panel.example.com", service); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actions, []string{"stop"}) {
		t.Fatalf("service calls = %v, want [stop]", actions)
	}
}
