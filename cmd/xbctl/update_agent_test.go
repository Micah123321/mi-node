package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/updateagent"
)

func TestAutoInstallCommandConfigAndNoCredentialArguments(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("xbctl requires root")
	}
	old := autoInstallUpdateAgent
	defer func() { autoInstallUpdateAgent = old }()
	var got string
	autoInstallUpdateAgent = func(_ context.Context, path string) error { got = path; return nil }
	if err := runUpdateAgent([]string{"auto-install", "-config", "/custom/node.yml"}); err != nil || got != "/custom/node.yml" {
		t.Fatalf("auto-install config: %s %v", got, err)
	}
	for _, args := range [][]string{
		{"auto-install", "-config", ""}, {"auto-install", "-token", "fixture"}, {"auto-install", "-config", "/custom/node.yml", "fixture-ticket"},
	} {
		if err := runUpdateAgent(args); err == nil {
			t.Fatal("unexpected credential/positional input accepted")
		}
	}
}

func TestAutoEnrollmentConfigUsesLoadRootEnvResolution(t *testing.T) {
	for _, machine := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "machine"}[machine], func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "node.yml")
			t.Setenv("AUTO_ENROLL_FIXTURE_TOKEN", "resolved-fixture")
			text := "panel:\n  url: https://panel.example\n  node_id: 7\n  node_type: vless\n  token_env: AUTO_ENROLL_FIXTURE_TOKEN\n"
			if machine {
				text = "panel:\n  url: https://panel.example\nmachine:\n  machine_id: 42\n  token_env: AUTO_ENROLL_FIXTURE_TOKEN\n"
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			root, err := config.LoadRoot(path)
			if err != nil {
				t.Fatal(err)
			}
			instances, err := root.NormalizeInstances()
			if err != nil {
				t.Fatal(err)
			}
			_, auth, err := updateagent.SelectAutoAuthority(instances, "")
			if err != nil || auth["token"] != "resolved-fixture" {
				t.Fatalf("env resolution: %v", err)
			}
			if machine && auth["machine_id"] != 42 {
				t.Fatal("machine ID lost")
			}
		})
	}
}
