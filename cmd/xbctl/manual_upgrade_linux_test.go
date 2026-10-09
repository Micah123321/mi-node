//go:build linux

package main

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/updateagent"
)

func TestManualCanonicalLockMatchesShell(t *testing.T) {
	e := manualFixture(t)
	alias := filepath.Join(e.Dir, "alias")
	if err := os.Symlink(e.Paths[0], alias); err != nil {
		t.Fatal(err)
	}
	unlock, err := updateagent.Lock(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(e.Paths[0])))
	lockPath := filepath.Join(e.Dir, ".mi-node-update-"+key+".lock")
	if err := exec.Command("flock", "-n", lockPath, "true").Run(); err == nil {
		t.Fatal("shell did not contend on canonical lock")
	}
	if other, err := updateagent.Lock(e.Paths[0]); err == nil {
		other()
		t.Fatal("alias bypassed lock")
	}
}

func TestInstallerManualTransaction(t *testing.T) {
	for _, tool := range []string{"bash", "jq", "flock", "sync"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skip(tool + " missing")
		}
	}
	source, err := os.ReadFile("../../install.sh")
	if err != nil {
		t.Fatal(err)
	}
	text := string(source)
	start := strings.Index(text, "upgrade_journal() {")
	end := strings.Index(text, "\nperform_uninstall()")
	if start < 0 || end < start {
		t.Fatal("installer upgrade functions missing")
	}
	for _, scenario := range []string{"success", "inactive", "restart", "second-rename", "unresolved", "corrupt", "rollback-failed", "busy"} {
		t.Run(scenario, func(t *testing.T) {
			e := manualFixture(t)
			state := filepath.Join(e.Dir, "state")
			os.Mkdir(state, 0700)
			if scenario == "unresolved" || scenario == "rollback-failed" {
				tx := updateagent.NewEngine()
				tx.Dir = state
				if err := tx.Save(&updateagent.Transaction{Resolved: scenario == "rollback-failed", Phase: "rollback_failed"}); err != nil {
					t.Fatal(err)
				}
			}
			if scenario == "corrupt" {
				os.WriteFile(filepath.Join(state, "transaction.json"), []byte("{"), 0600)
			}
			if scenario == "busy" {
				unlock, err := updateagent.Lock(e.Paths[0])
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			// Only the extracted upgrade functions run; paths and all service effects are injected.
			script := "set -euo pipefail\n" + strings.ReplaceAll(text[start:end], "/etc/mi-node/update-agent", state) + `
check_root() { :; }
ensure_supported_init() { :; }
detect_arch() { :; }
log_error() { echo "$*" >&2; }
log_info() { :; }
cleanup() { [ -z "${TMP_DIR:-}" ] || rm -rf "$TMP_DIR"; }
detect_current_state() { SERVICE_WAS_ACTIVE=1; if [ "$SCENARIO" = inactive ]; then SERVICE_WAS_ACTIVE=0; fi; }
stage_binary() { printf '#!/bin/sh\nexit 0\n# new node\n' > "$TMP_DIR/mi-node"; chmod 755 "$TMP_DIR/mi-node"; }
stage_xbctl() { printf '#!/bin/sh\nexit 0\n# new cli\n' > "$TMP_DIR/xbctl"; chmod 755 "$TMP_DIR/xbctl"; }
ln() { :; }
service_is_active() { return 0; }
restarts=0
service_restart() { restarts=$((restarts+1)); if [ "$SCENARIO" = restart ] && [ "$restarts" -eq 1 ]; then return 1; fi; return 0; }
failed_move=0
mv() {
 if [ "$SCENARIO" = second-rename ] && [ "$3" = "$CLI_PATH" ] && [ "$failed_move" -eq 0 ]; then failed_move=1; return 1; fi
 command mv "$@"
}
perform_upgrade
`
			cmd := exec.Command("bash", "-c", script)
			cmd.Env = append(os.Environ(), "BINARY_PATH="+e.Paths[0], "CLI_PATH="+e.Paths[1], "RELEASE_VERSION=latest", "SCENARIO="+scenario)
			out, err := cmd.CombinedOutput()
			success := scenario == "success" || scenario == "inactive"
			if success != (err == nil) {
				t.Fatalf("success=%v err=%v output=%s", success, err, out)
			}
			for _, p := range e.Paths {
				b, err := os.ReadFile(p)
				if err != nil {
					t.Fatal(err)
				}
				if success {
					if !strings.Contains(string(b), "# new") {
						t.Fatalf("not upgraded: %s", b)
					}
				} else if string(b) != "old" {
					t.Fatalf("old binary lost: %s", b)
				}
			}
			if success || scenario == "restart" || scenario == "second-rename" {
				journal := updateagent.NewEngine()
				journal.Dir = state
				tx, err := journal.Load()
				if err != nil {
					t.Fatal(err)
				}
				if !tx.Manual || !tx.Resolved {
					t.Fatalf("journal not finalized: %+v", tx)
				}
			}
		})
	}
}
