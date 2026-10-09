package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/updateagent"
)

func manualFixture(t *testing.T) *updateagent.Engine {
	t.Helper()
	e := updateagent.NewEngine()
	e.Dir = t.TempDir()
	e.Paths = [2]string{filepath.Join(e.Dir, "mi-node"), filepath.Join(e.Dir, "xbctl")}
	for _, p := range e.Paths {
		if err := os.WriteFile(p, []byte("old"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	manualServiceEngine(e, func() error { return nil }, func() string { return "running" })
	return e
}

func TestManualUpgradeTransaction(t *testing.T) {
	for _, scenario := range []string{"latest", "version", "second-rename", "restart", "download", "validation", "unresolved", "rollback-failed", "corrupt", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			e := manualFixture(t)
			held, downloaded, released := false, 0, false
			lock := func(p string) (func(), error) {
				if p != e.Paths[0] {
					t.Fatalf("wrong lock path %q", p)
				}
				if scenario == "busy" {
					return nil, errors.New("busy")
				}
				held = true
				return func() { held = false; released = true }, nil
			}
			if scenario == "unresolved" || scenario == "rollback-failed" {
				tx := &updateagent.Transaction{Resolved: scenario == "rollback-failed", Phase: "rollback_failed"}
				if err := e.Save(tx); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt" {
				os.WriteFile(filepath.Join(e.Dir, "transaction.json"), []byte("{"), 0600)
			}
			if scenario == "second-rename" {
				e.Rename = func(a, b string) error {
					if b == e.Paths[1] && strings.Contains(filepath.Base(a), "-manual-") {
						return errors.New("rename failed")
					}
					return os.Rename(a, b)
				}
			}
			calls := 0
			if scenario == "restart" {
				e.Restart = func(context.Context) error {
					calls++
					if calls == 1 {
						return errors.New("restart failed")
					}
					return nil
				}
			}
			args := []string(nil)
			versionPath := "/latest/download/"
			if scenario == "version" {
				args = []string{"--version", "v1.2.3"}
				versionPath = "/download/v1.2.3/"
			}
			err := manualUpgrade(args, e, lock, func(url, dest string) error {
				if !held {
					t.Fatal("download without lock")
				}
				downloaded++
				if !strings.Contains(url, versionPath) {
					t.Fatalf("wrong release URL: %s", url)
				}
				if scenario == "download" && downloaded == 2 {
					return errors.New("download failed")
				}
				return os.WriteFile(dest, []byte("new"), 0755)
			}, func(string, ...string) error {
				if scenario == "validation" {
					return errors.New("invalid binary")
				}
				return nil
			})
			success := scenario == "latest" || scenario == "version"
			if success != (err == nil) {
				t.Fatalf("success=%v err=%v", success, err)
			}
			if held || (scenario != "busy" && !released) {
				t.Fatal("lock leaked")
			}
			if (scenario == "unresolved" || scenario == "rollback-failed" || scenario == "corrupt" || scenario == "busy") && downloaded != 0 {
				t.Fatal("download before transaction gate")
			}
			want := "old"
			if success {
				want = "new"
			}
			for _, p := range e.Paths {
				b, err := os.ReadFile(p)
				if err != nil || string(b) != want {
					t.Fatalf("binary %s = %q, %v", p, b, err)
				}
			}
			if success || scenario == "second-rename" || scenario == "restart" {
				tx, err := e.Load()
				if err != nil {
					t.Fatal(err)
				}
				if !tx.Manual || !tx.Resolved {
					t.Fatalf("unfinished manual transaction: %+v", tx)
				}
				if !success && tx.Phase != "rolled_back" {
					t.Fatalf("phase=%s", tx.Phase)
				}
			}
		})
	}
}

func TestManualOpenRCHealth(t *testing.T) {
	e := manualFixture(t)
	for _, state := range []string{"running", "active", "stopped", "unknown"} {
		manualServiceEngine(e, func() error { return nil }, func() string { return state })
		_, err := e.Health(context.Background(), "latest", updateagent.HealthSnapshot{}, false)
		if (state == "running" || state == "active") != (err == nil) {
			t.Fatalf("state %s: %v", state, err)
		}
	}
}
