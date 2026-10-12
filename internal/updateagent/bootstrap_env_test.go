package updateagent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
)

func clearBootstrapTestEnvironment(t *testing.T) {
	t.Helper()
	for _, name := range bootstrapConfigEnvironment {
		t.Setenv(name, "")
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
	}
}

func writeBootstrapTestConfig(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

// Apply EnvironmentFile's double-quote rules, including literal multiline values.
// Go string unquoting would incorrectly interpret literal backslash-n sequences.
func readBootstrapTestEnvironment(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var env []string
	for len(data) > 0 {
		eq := bytes.IndexByte(data, '=')
		if eq < 1 || eq+1 >= len(data) || data[eq+1] != '"' {
			t.Fatal("invalid environment assignment")
		}
		name := string(data[:eq])
		data = data[eq+2:]
		var value strings.Builder
		closed := false
		for len(data) > 0 {
			c := data[0]
			data = data[1:]
			if c == '"' {
				closed = true
				break
			}
			if c == '\\' && len(data) > 0 && strings.ContainsRune("\"\\$\x60\n", rune(data[0])) {
				c = data[0]
				data = data[1:]
				if c == '\n' {
					continue
				}
			}
			value.WriteByte(c)
		}
		if !closed || len(data) == 0 || data[0] != '\n' {
			t.Fatal("unterminated environment assignment")
		}
		data = data[1:]
		env = append(env, name+"="+value.String())
	}
	return env
}

type bootstrapChildConfig struct {
	Root      *config.RootConfig
	Unrelated string
}

func TestBootstrapEnvironmentChild(t *testing.T) {
	if os.Getenv("BOOTSTRAP_TEST_CHILD") != "1" {
		return
	}
	root, err := config.LoadRoot(os.Getenv("BOOTSTRAP_TEST_CONFIG"))
	if err != nil {
		os.Exit(2)
	}
	if err := json.NewEncoder(os.Stdout).Encode(bootstrapChildConfig{Root: root, Unrelated: os.Getenv("BOOTSTRAP_UNRELATED")}); err != nil {
		os.Exit(3)
	}
	os.Exit(0)
}

func assertBootstrapChildConfig(t *testing.T, configPath, environmentPath string) {
	t.Helper()
	want, err := config.LoadRoot(configPath)
	if err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "-test.run=^TestBootstrapEnvironmentChild$")
	// Deliberately do not inherit os.Environ: this is a fresh service process.
	cmd.Env = append(readBootstrapTestEnvironment(t, environmentPath), "BOOTSTRAP_TEST_CHILD=1", "BOOTSTRAP_TEST_CONFIG="+configPath)
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("isolated config load failed: %v", err)
	}
	var got bootstrapChildConfig
	if err := json.Unmarshal(output, &got); err != nil {
		t.Fatal(err)
	}
	if got.Unrelated != "" || !reflect.DeepEqual(got.Root, want) {
		t.Fatal("isolated config differs from the main process or unrelated environment leaked")
	}
}

func TestBootstrapEnvironmentIndependentConfigLoad(t *testing.T) {
	for _, tc := range []struct {
		name, config string
		env          map[string]string
	}{
		{
			name: "custom multi-instance panel and machine references",
			config: "panel: {token_env: BOOTSTRAP_UNUSED_DEFAULT}\ninstances:\n" +
				"  - panel: {url: https://panel.example, token_env: BOOTSTRAP_PANEL_TOKEN, node_id: 7}\n" +
				"  - panel: {url: https://machine.example}\n    machine: {machine_id: 8, token_env: BOOTSTRAP_MACHINE_TOKEN}\n",
			env: map[string]string{
				"BOOTSTRAP_PANEL_TOKEN":    " \tpanel 'quote' \"double\" \\ $HOME $" + "{var} %n #;=\x60command\x60\nsecond\rline\\n\\\n ",
				"BOOTSTRAP_MACHINE_TOKEN":  "machine-fixture-token",
				"BOOTSTRAP_UNUSED_DEFAULT": "unused-fixture-token",
			},
		},
		{
			name:   "multi-node custom panel reference",
			config: "panel: {url: https://panel.example, token_env: BOOTSTRAP_PANEL_TOKEN}\nnodes: [{node_id: 7}, {node_id: 8}]\n",
			env:    map[string]string{"BOOTSTRAP_PANEL_TOKEN": "multi-node-token"},
		},
		{
			name:   "environment overrides and alias precedence",
			config: "panel: {url: https://old.example, token_env: BOOTSTRAP_PANEL_TOKEN, node_id: 7}\n",
			env: map[string]string{
				"apiHost": "https://preferred.example", "API_HOST": "https://other.example",
				"apiKey": "preferred-token", "API_KEY": "other-token", "NODE_ID": "9",
				"nodeType": "vless", "KERNEL_TYPE": "xray", "CERT_FILE": "/fixture/cert.pem",
				"KEY_FILE": "/fixture/key.pem", "DOMAIN": "tls.example", "LOG_LEVEL": "debug",
				"BOOTSTRAP_PANEL_TOKEN": "lower-priority-token",
			},
		},
		{
			name:   "machine environment overrides",
			config: "panel: {url: https://panel.example}\nmachine: {machine_id: 1, token_env: BOOTSTRAP_MACHINE_TOKEN}\n",
			env:    map[string]string{"MACHINE_ID": "11", "MACHINE_TOKEN": "override-machine", "BOOTSTRAP_MACHINE_TOKEN": "lower-priority-machine"},
		},
		{
			name: "configuration entirely from environment",
			env:  map[string]string{"API_HOST": "https://env.example", "API_KEY": "env-token", "NODE_ID": "12"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clearBootstrapTestEnvironment(t)
			for name, value := range tc.env {
				t.Setenv(name, value)
			}
			t.Setenv("BOOTSTRAP_UNRELATED", "must-not-leak")
			configPath := filepath.Join(t.TempDir(), "node.yml")
			if tc.config != "" {
				writeBootstrapTestConfig(t, configPath, tc.config)
			}
			state := t.TempDir()
			if err := writeBootstrapEnvironment(configPath, state); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state, "bootstrap", "environment.env")
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"BOOTSTRAP_UNRELATED", "must-not-leak", "BOOTSTRAP_UNUSED_DEFAULT", "unused-fixture-token"} {
				if bytes.Contains(data, []byte(forbidden)) {
					t.Fatal("unreferenced environment was copied")
				}
			}
			assertBootstrapChildConfig(t, configPath, path)
		})
	}
}

func TestBootstrapEnvironmentRotationAndPermissions(t *testing.T) {
	clearBootstrapTestEnvironment(t)
	t.Setenv("BOOTSTRAP_PANEL_TOKEN", "old-fixture-token")
	t.Setenv("BOOTSTRAP_NEXT_TOKEN", "new-fixture-token")
	t.Setenv("API_KEY", "old-override-token")
	configPath := filepath.Join(t.TempDir(), "node.yml")
	writeBootstrapTestConfig(t, configPath, "panel: {url: https://panel.example, token_env: BOOTSTRAP_PANEL_TOKEN, node_id: 7}\n")
	state := t.TempDir()
	path := filepath.Join(state, "bootstrap", "environment.env")
	if err := writeBootstrapEnvironment(configPath, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Dir(path), 0777); err != nil {
		t.Fatal(err)
	}
	if err := writeBootstrapEnvironment(configPath, state); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("unchanged snapshot rewritten")
	}
	if runtime.GOOS != "windows" {
		dir, err := os.Stat(filepath.Dir(path))
		if err != nil || dir.Mode().Perm() != 0700 || after.Mode().Perm() != 0600 {
			t.Fatal("snapshot permissions not repaired")
		}
	}
	if err := os.Unsetenv("API_KEY"); err != nil {
		t.Fatal(err)
	}
	writeBootstrapTestConfig(t, configPath, "panel: {url: https://panel.example, token_env: BOOTSTRAP_NEXT_TOKEN, node_id: 7}\n")
	if err := writeBootstrapEnvironment(configPath, state); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte("old-")) || bytes.Contains(data, []byte("BOOTSTRAP_PANEL_TOKEN=")) || bytes.Contains(data, []byte("API_KEY=")) {
		t.Fatal("rotated snapshot retained stale credentials")
	}
	assertBootstrapChildConfig(t, configPath, path)
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".update-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatal("snapshot temporary files retained")
	}
}

func TestBootstrapEnvironmentRejectsUnsafeStorage(t *testing.T) {
	clearBootstrapTestEnvironment(t)
	t.Setenv("BOOTSTRAP_PANEL_TOKEN", "storage-fixture-token")
	configPath := filepath.Join(t.TempDir(), "node.yml")
	writeBootstrapTestConfig(t, configPath, "panel: {url: https://panel.example, token_env: BOOTSTRAP_PANEL_TOKEN, node_id: 7}\n")
	for _, target := range []string{"state", "bootstrap", "environment.env"} {
		t.Run(target, func(t *testing.T) {
			base, outside := t.TempDir(), t.TempDir()
			state := filepath.Join(base, "state")
			path := state
			if target != "state" {
				path = filepath.Join(state, "bootstrap")
			}
			if target == "environment.env" {
				path = filepath.Join(path, target)
			}
			if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skip("symlink creation unavailable on this platform")
			}
			if err := writeBootstrapEnvironment(configPath, state); err == nil {
				t.Fatal("symlink storage accepted")
			}
			entries, err := os.ReadDir(outside)
			if err != nil || len(entries) != 0 {
				t.Fatal("symlink target modified")
			}
		})
	}
}

func TestBootstrapEnvironmentFailureDoesNotStartUnitsOrRevealSecrets(t *testing.T) {
	clearBootstrapTestEnvironment(t)
	t.Setenv("BAD-NAME", "secret-fixture-value")
	configPath := filepath.Join(t.TempDir(), "node.yml")
	writeBootstrapTestConfig(t, configPath, "panel: {url: https://panel.example, token_env: BAD-NAME, node_id: 7}\n")
	called := false
	ctl := func(context.Context, ...string) (string, error) {
		called = true
		return "", nil
	}
	err := bootstrapUnits(context.Background(), t.TempDir(), configPath, t.TempDir(), ctl)
	if err == nil || called || strings.Contains(err.Error(), "secret-fixture-value") || strings.Contains(err.Error(), "BAD-NAME") {
		t.Fatal("invalid snapshot started systemd or revealed credentials")
	}
	for _, value := range []string{"\x00", "\xff", "\uFEFF", "\uFDD0", "\uFFFF", "\U0010FFFF"} {
		if validBootstrapEnvValue(value) {
			t.Fatal("systemd-incompatible value accepted")
		}
	}
}
