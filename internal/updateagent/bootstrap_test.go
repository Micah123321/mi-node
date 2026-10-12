package updateagent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestBootstrapIndependentRetryUnitsAndIdempotence(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(t.TempDir(), "custom config%$", "node.yml")
	clearBootstrapTestEnvironment(t)
	writeBootstrapTestConfig(t, configPath, "panel: {url: https://panel.example, token_env: BOOTSTRAP_PANEL_TOKEN, node_id: 7}\n")
	t.Setenv("BOOTSTRAP_PANEL_TOKEN", "unit-fixture-secret")
	stateDirectory := filepath.Join(t.TempDir(), "state%$")
	var calls []string
	ctl := func(_ context.Context, args ...string) (string, error) {
		if _, err := os.Stat(filepath.Join(stateDirectory, "bootstrap", "environment.env")); err != nil {
			t.Fatal("systemd invoked before environment snapshot exists")
		}
		calls = append(calls, strings.Join(args, " "))
		return "", nil
	}
	if err := bootstrapUnits(context.Background(), directory, configPath, stateDirectory, ctl); err != nil {
		t.Fatal(err)
	}
	servicePath := filepath.Join(directory, bootstrapService)
	service, _ := os.ReadFile(servicePath)
	timer, _ := os.ReadFile(filepath.Join(directory, bootstrapTimer))
	for _, part := range []string{"Type=oneshot", "ExecStart=/usr/local/bin/xbctl update-agent auto-install -config ", "EnvironmentFile=", "state%%$", "custom config%%$$", "TimeoutStartSec=0"} {
		if !strings.Contains(string(service), part) {
			t.Errorf("missing bootstrap unit contract %q", part)
		}
	}
	for _, part := range []string{"OnUnitInactiveSec=60s", "Unit=" + bootstrapService, "WantedBy=timers.target"} {
		if !strings.Contains(string(timer), part) {
			t.Errorf("missing retry %q", part)
		}
	}
	for _, forbidden := range []string{"unit-fixture-secret", "EnvironmentFile=-", "credentials.env", "token=", "enrollment-file", "--ticket", "PartOf=mi-node.service", "BindsTo=mi-node.service", "ExecStop="} {
		if strings.Contains(string(service), forbidden) {
			t.Errorf("undesired bootstrap coupling %q", forbidden)
		}
	}
	joined := strings.Join(calls, "\n")
	if !strings.Contains(joined, "start --no-block "+bootstrapService) || !strings.Contains(joined, "enable --now "+bootstrapTimer) {
		t.Fatal("bootstrap not independently scheduled")
	}
	before, _ := os.Stat(servicePath)
	calls = nil
	if err := bootstrapUnits(context.Background(), directory, configPath, stateDirectory, ctl); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Stat(servicePath)
	if !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || strings.Contains(strings.Join(calls, " "), "daemon-reload") {
		t.Fatal("unchanged bootstrap units rewritten")
	}
}

func TestBootstrapEnvironmentPathNativeSystemdParser(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux parser required")
	}
	analyzer, err := exec.LookPath("systemd-analyze")
	if err != nil {
		t.Skip("systemd-analyze unavailable")
	}
	for _, directory := range []string{"plain", "space percent% dollar$"} {
		t.Run(directory, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), directory, "environment.env")
			unit, err := bootstrapServiceUnit("/etc/mi-node/config.yml", path)
			if err != nil {
				t.Fatal(err)
			}
			// Use an available executable for parser validation without running the bootstrap.
			unit = strings.Replace(unit, "ExecStart=/usr/local/bin/xbctl update-agent auto-install -config \"/etc/mi-node/config.yml\"", "ExecStart=/bin/true", 1)
			file := filepath.Join(t.TempDir(), "bootstrap-parser-check.service")
			if err := os.WriteFile(file, []byte(unit), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			output, err := exec.CommandContext(ctx, analyzer, "verify", "--man=no", file).CombinedOutput()
			if err != nil || strings.Contains(string(output), "EnvironmentFile= path is not absolute") {
				t.Fatalf("systemd rejected generated bootstrap unit: %v: %s", err, output)
			}
		})
	}
}

func TestBootstrapRejectsMultilineConfigPath(t *testing.T) {
	for _, path := range []string{"/etc/fixture\nExecStart=bad", "/etc/fixture\r", "/etc/fixture\x00"} {
		if _, err := bootstrapServiceUnit(path, filepath.Join(StateDir, "bootstrap", "environment.env")); err == nil {
			t.Fatal("invalid unit path accepted")
		}
	}
}

func TestAutomaticPlatformSupport(t *testing.T) {
	for _, tc := range []struct {
		os, arch, init       string
		container, supported bool
	}{
		{"linux", "amd64", "systemd", false, true}, {"linux", "arm64", "systemd", false, true},
		{"linux", "386", "systemd", false, false}, {"windows", "amd64", "systemd", false, false},
		{"linux", "amd64", "openrc", false, false}, {"linux", "amd64", "other", false, false},
		{"linux", "amd64", "systemd", true, false},
	} {
		reason, init, container := platformCapability(tc.os, tc.arch, tc.init, tc.container)
		if (reason == "") != tc.supported || init != tc.init || container != tc.container {
			t.Errorf("platform %+v: %s", tc, reason)
		}
	}
}
