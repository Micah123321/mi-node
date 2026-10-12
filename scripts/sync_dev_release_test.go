package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

const syncVersion = "v20261012.38249125239.1"
const syncCommit = "21e13f93d4aa9dadaf8367db4842c54435dd1678"

var syncNames = []string{"mi-node-linux-amd64", "mi-node-linux-arm64", "xbctl-linux-amd64", "xbctl-linux-arm64", "node-release.json"}

type syncHarness struct {
	dir string
	env map[string]string
}

func putSyncFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0755); err != nil {
		t.Fatal(err)
	}
}

func newSyncHarness(t *testing.T) *syncHarness {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("execute Linux bash behavior tests on Linux (or the cross-compiled test binary under WSL)")
	}
	for _, name := range []string{"bash", "jq", "sha256sum"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatal(err)
		}
	}
	h := &syncHarness{dir: t.TempDir(), env: map[string]string{}}
	h.env = map[string]string{
		"PATH": filepath.Join(h.dir, "bin") + ":" + os.Getenv("PATH"), "LC_ALL": "C",
		"GITHUB_REPOSITORY": "owner/repo", "GITHUB_EVENT_NAME": "push",
		"VERSION": syncVersion, "GITHUB_SHA": syncCommit, "GITHUB_REF_TYPE": "branch",
		"GITHUB_OUTPUT": filepath.Join(h.dir, "output"), "FAKE_ROOT": h.dir,
	}
	for _, name := range syncNames[:4] {
		putSyncFile(t, filepath.Join(h.dir, "dist", name), []byte(name+"\nversion="+syncVersion+"\n"))
	}
	if err := run([]string{"--version", syncVersion, "--repository", "owner/repo", "--dir", filepath.Join(h.dir, "dist"), "--output", filepath.Join(h.dir, "dist", "node-release.json")}, noEnv, time.Time{}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	assets := []map[string]any{}
	for _, name := range syncNames {
		data := h.read(t, "dist/"+name)
		putSyncFile(t, filepath.Join(h.dir, "fixed-assets", name), data)
		assets = append(assets, map[string]any{"name": name, "size": len(data)})
	}
	data, err := json.Marshal(map[string]any{"tagName": syncVersion, "isDraft": false, "isPrerelease": false, "assets": assets, "body": "immutable fixed notes", "targetCommitish": syncCommit})
	if err != nil {
		t.Fatal(err)
	}
	putSyncFile(t, filepath.Join(h.dir, "fixed.json"), data)
	putSyncFile(t, filepath.Join(h.dir, "source.json"), data)
	putSyncFile(t, filepath.Join(h.dir, "fixed.exists"), nil)
	putSyncFile(t, filepath.Join(h.dir, "latest"), []byte(syncVersion))
	// This unrelated dist file must never become a release asset.
	putSyncFile(t, filepath.Join(h.dir, "dist", "extra-debug.txt"), []byte("extra"))
	script, err := os.ReadFile("sync-dev-release.sh")
	if err != nil {
		t.Fatal(err)
	}
	putSyncFile(t, filepath.Join(h.dir, "scripts", "sync-dev-release.sh"), script)
	putSyncFile(t, filepath.Join(h.dir, "bin", "gh"), []byte(fakeReleaseGH))
	putSyncFile(t, filepath.Join(h.dir, "bin", "git"), []byte(`#!/usr/bin/env bash
set -euo pipefail
jq -cn --args '$ARGS.positional' -- "$@" >> "$FAKE_ROOT/git.jsonl"
[[ "$*" == "ls-remote --tags origin refs/tags/$VERSION" ]] || exit 90
if [[ "${FAKE_EXISTING_TAG:-}" == true ]]; then printf '%s\trefs/tags/%s\n' "$GITHUB_SHA" "$VERSION"; fi
`))
	return h
}

func (h *syncHarness) read(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func (h *syncHarness) command(args ...string) ([]byte, error) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = h.dir
	for key, value := range h.env {
		cmd.Env = append(cmd.Env, key+"="+value)
	}
	return cmd.CombinedOutput()
}

func (h *syncHarness) sync() ([]byte, error) {
	return h.command("bash", "scripts/sync-dev-release.sh", h.env["VERSION"], h.env["GITHUB_SHA"], "dist")
}

func (h *syncHarness) calls(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(h.dir, "calls.jsonl"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range bytes.Split(bytes.TrimSpace(data), []byte("\n")) {
		var call []string
		if err := json.Unmarshal(line, &call); err != nil {
			t.Fatal(err)
		}
		calls = append(calls, call)
	}
	return calls
}

func (h *syncHarness) existingDev(t *testing.T) {
	putSyncFile(t, filepath.Join(h.dir, "dev.exists"), nil)
	putSyncFile(t, filepath.Join(h.dir, "dev.notes"), []byte("old dev notes"))
	putSyncFile(t, filepath.Join(h.dir, "dev.tag"), []byte(strings.Repeat("5", 40)))
	for _, name := range syncNames {
		putSyncFile(t, filepath.Join(h.dir, "dev-assets", name), []byte("old "+name))
	}
}

func (h *syncHarness) snapshotFixed(t *testing.T) map[string][]byte {
	result := map[string][]byte{"fixed.json": h.read(t, "fixed.json")}
	for _, name := range syncNames {
		result["fixed-assets/"+name] = h.read(t, "fixed-assets/"+name)
	}
	return result
}

func (h *syncHarness) assertFixed(t *testing.T, before map[string][]byte) {
	t.Helper()
	if !reflect.DeepEqual(h.snapshotFixed(t), before) {
		t.Fatal("alias sync changed immutable fixed assets or metadata")
	}
	for _, call := range h.calls(t) {
		if call[1] != "view" && call[2] != "dev" {
			t.Fatalf("alias sync mutated another release: %q", call)
		}
	}
}

func (h *syncHarness) assertDev(t *testing.T, expectedTag string) {
	t.Helper()
	for _, name := range syncNames {
		if !bytes.Equal(h.read(t, "dev-assets/"+name), h.read(t, "fixed-assets/"+name)) {
			t.Fatalf("compatibility asset differs from fixed source: %s", name)
		}
	}
	entries, err := os.ReadDir(filepath.Join(h.dir, "dev-assets"))
	if err != nil || len(entries) != 5 {
		t.Fatalf("wrong compatibility asset whitelist: %v %v", entries, err)
	}
	notes := string(h.read(t, "dev.notes"))
	for _, want := range []string{"mutable", "Source fixed release: " + syncVersion, "Source commit: " + syncCommit, "https://github.com/owner/repo/releases/tag/" + syncVersion} {
		if !strings.Contains(strings.ToLower(notes), strings.ToLower(want)) {
			t.Fatalf("missing source identity %q in notes: %s", want, notes)
		}
	}
	if string(h.read(t, "dev.tag")) != expectedTag || string(h.read(t, "dev.prerelease")) != "true" || string(h.read(t, "dev.draft")) != "false" || string(h.read(t, "latest")) != syncVersion {
		t.Fatal("dev changed the historical tag or competed with fixed latest")
	}
}

func TestSyncDevReleaseCreationAndReplacement(t *testing.T) {
	for _, scenario := range []string{"new-release-and-tag", "new-release-with-historical-tag", "existing-release", "manual-dispatch"} {
		t.Run(scenario, func(t *testing.T) {
			h := newSyncHarness(t)
			expectedTag := syncCommit
			switch scenario {
			case "new-release-with-historical-tag":
				expectedTag = strings.Repeat("5", 40)
				putSyncFile(t, filepath.Join(h.dir, "dev.tag"), []byte(expectedTag))
			case "existing-release":
				h.existingDev(t)
				expectedTag = strings.Repeat("5", 40)
			case "manual-dispatch":
				h.env["GITHUB_EVENT_NAME"] = "workflow_dispatch"
			}
			before := h.snapshotFixed(t)
			if output, err := h.sync(); err != nil {
				t.Fatalf("%v: %s", err, output)
			}
			h.assertFixed(t, before)
			h.assertDev(t, expectedTag)
			for _, call := range h.calls(t) {
				if call[1] == "edit" && containsSyncArg(call, "--target") {
					t.Fatalf("release edit must not retarget a git tag: %q", call)
				}
			}
		})
	}
}

func containsSyncArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func mutateSyncJSON(t *testing.T, h *syncHarness, path string, mutate func(map[string]any)) {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(h.read(t, path), &object); err != nil {
		t.Fatal(err)
	}
	mutate(object)
	data, err := json.Marshal(object)
	if err != nil {
		t.Fatal(err)
	}
	putSyncFile(t, filepath.Join(h.dir, path), data)
}

func TestSyncDevReleaseGatesAndUploadFailure(t *testing.T) {
	for _, scenario := range []string{"not-latest", "pull-request", "draft-source", "prerelease-source", "missing-source-asset", "wrong-source-size", "latest-query-failed", "upload-failed-new", "upload-failed-existing"} {
		t.Run(scenario, func(t *testing.T) {
			h := newSyncHarness(t)
			wantError := true
			switch scenario {
			case "not-latest":
				putSyncFile(t, filepath.Join(h.dir, "latest"), []byte("v20261013.1.1"))
				wantError = false
			case "pull-request":
				h.env["GITHUB_EVENT_NAME"] = "pull_request"
				wantError = false
			case "draft-source":
				mutateSyncJSON(t, h, "fixed.json", func(m map[string]any) { m["isDraft"] = true })
			case "prerelease-source":
				mutateSyncJSON(t, h, "fixed.json", func(m map[string]any) { m["isPrerelease"] = true })
			case "missing-source-asset":
				mutateSyncJSON(t, h, "fixed.json", func(m map[string]any) { m["assets"] = m["assets"].([]any)[:4] })
			case "wrong-source-size":
				mutateSyncJSON(t, h, "fixed.json", func(m map[string]any) { m["assets"].([]any)[0].(map[string]any)["size"] = 1 })
			case "latest-query-failed":
				h.env["FAKE_FAIL_LATEST"] = "true"
			case "upload-failed-new", "upload-failed-existing":
				h.env["FAKE_FAIL_UPLOAD"] = "dev"
				if scenario == "upload-failed-existing" {
					h.existingDev(t)
				}
			}
			before := h.snapshotFixed(t)
			output, err := h.sync()
			if (err != nil) != wantError {
				t.Fatalf("wrong outcome: %v %s", err, output)
			}
			h.assertFixed(t, before)
			for _, call := range h.calls(t) {
				if strings.HasPrefix(scenario, "upload-failed") {
					if call[1] == "edit" && containsSyncArg(call, "--notes") {
						t.Fatal("failed upload advertised new source notes")
					}
				} else if call[1] != "view" {
					t.Fatalf("gate failure caused a release mutation: %q", call)
				}
			}
			if scenario == "upload-failed-existing" && string(h.read(t, "dev.notes")) != "old dev notes" {
				t.Fatal("failed upload changed existing source notes")
			}
			if scenario == "pull-request" && len(h.calls(t)) != 0 {
				t.Fatal("PR invoked GitHub release tooling")
			}
		})
	}
}

func TestSyncDevReleaseRejectsInvalidInputsBeforeGitHub(t *testing.T) {
	cases := map[string]func(*testing.T, *syncHarness){
		"mutable-version":      func(t *testing.T, h *syncHarness) { h.env["VERSION"] = "dev" },
		"leading-zero-version": func(t *testing.T, h *syncHarness) { h.env["VERSION"] = "v01.2.3" },
		"version-suffix":       func(t *testing.T, h *syncHarness) { h.env["VERSION"] = "v1.2.3-rc1" },
		"short-sha":            func(t *testing.T, h *syncHarness) { h.env["GITHUB_SHA"] = "1234567" },
		"bad-repository":       func(t *testing.T, h *syncHarness) { h.env["GITHUB_REPOSITORY"] = "owner/repo/other" },
		"malformed-json": func(t *testing.T, h *syncHarness) {
			putSyncFile(t, filepath.Join(h.dir, "dist/node-release.json"), []byte("{"))
		},
		"multiple-json-documents": func(t *testing.T, h *syncHarness) {
			putSyncFile(t, filepath.Join(h.dir, "dist/node-release.json"), append(h.read(t, "dist/node-release.json"), []byte("\n{}")...))
		},
		"changed-binary": func(t *testing.T, h *syncHarness) {
			putSyncFile(t, filepath.Join(h.dir, "dist/mi-node-linux-amd64"), []byte("different bytes"))
		},
	}
	mutations := map[string]func(map[string]any){
		"manifest-version":     func(m map[string]any) { m["version"] = "v1.2.3" },
		"extra-schema-field":   func(m map[string]any) { m["extra"] = true },
		"wrong-os":             func(m map[string]any) { m["os"] = "windows" },
		"wrong-protocol":       func(m map[string]any) { m["min_agent_protocol"] = 2 },
		"extra-artifact-field": func(m map[string]any) { m["artifacts"].([]any)[0].(map[string]any)["extra"] = true },
		"alias-url": func(m map[string]any) {
			m["artifacts"].([]any)[0].(map[string]any)["https_url"] = "https://github.com/owner/repo/releases/download/dev/mi-node-linux-amd64"
		},
		"wrong-checksum":     func(m map[string]any) { m["artifacts"].([]any)[0].(map[string]any)["sha256"] = strings.Repeat("0", 64) },
		"wrong-size":         func(m map[string]any) { m["artifacts"].([]any)[0].(map[string]any)["size_bytes"] = 1 },
		"duplicate-artifact": func(m map[string]any) { a := m["artifacts"].([]any); a[1] = a[0] },
	}
	for name, mutate := range mutations {
		cases[name] = func(t *testing.T, h *syncHarness) { mutateSyncJSON(t, h, "dist/node-release.json", mutate) }
	}
	for _, name := range syncNames {
		for _, empty := range []bool{false, true} {
			cases[fmt.Sprintf("%s/empty=%t", name, empty)] = func(t *testing.T, h *syncHarness) {
				path := filepath.Join(h.dir, "dist", name)
				if empty {
					putSyncFile(t, path, nil)
				} else if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	for name, prepare := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSyncHarness(t)
			prepare(t, h)
			if output, err := h.sync(); err == nil {
				t.Fatalf("invalid input succeeded: %s", output)
			}
			if len(h.calls(t)) != 0 {
				t.Fatal("invalid input reached GitHub")
			}
		})
	}
}

func TestWorkflowPublicationBehavior(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	var publish, syncRun string
	for _, entry := range workflow["jobs"].(map[string]any)["release"].(map[string]any)["steps"].([]any) {
		step := entry.(map[string]any)
		if step["id"] == "publish" {
			publish = step["run"].(string)
		}
		if step["name"] == "Sync mutable dev compatibility release" {
			syncRun = step["run"].(string)
		}
	}
	if publish == "" || syncRun == "" {
		t.Fatal("missing publication steps")
	}
	for _, scenario := range []string{"promoted", "old-slow-build", "existing-fixed", "existing-fixed-tag", "fixed-upload-failed", "fixed-promotion-failed", "not-selected-as-latest", "first-published-release", "latest-query-failed", "release-list-failed", "first-release-list-failed"} {
		t.Run(scenario, func(t *testing.T) {
			h := newSyncHarness(t)
			if scenario != "existing-fixed" {
				if err := os.Remove(filepath.Join(h.dir, "fixed.exists")); err != nil {
					t.Fatal(err)
				}
				if err := os.RemoveAll(filepath.Join(h.dir, "fixed-assets")); err != nil {
					t.Fatal(err)
				}
			}
			putSyncFile(t, filepath.Join(h.dir, "latest"), []byte("v20261011.1.1"))
			switch scenario {
			case "old-slow-build":
				putSyncFile(t, filepath.Join(h.dir, "latest"), []byte("v20261013.1.1"))
			case "existing-fixed-tag":
				h.env["FAKE_EXISTING_TAG"] = "true"
			case "fixed-upload-failed":
				h.env["FAKE_FAIL_UPLOAD"] = syncVersion
			case "fixed-promotion-failed":
				h.env["FAKE_FAIL_EDIT"] = syncVersion
			case "not-selected-as-latest":
				h.env["FAKE_IGNORE_PROMOTION"] = "true"
			case "first-published-release", "first-release-list-failed":
				putSyncFile(t, filepath.Join(h.dir, "latest"), nil)
				if scenario == "first-release-list-failed" {
					h.env["FAKE_FAIL_RELEASE_LIST"] = "true"
				}
			case "latest-query-failed", "release-list-failed":
				putSyncFile(t, filepath.Join(h.dir, "latest"), []byte("v20261013.1.1"))
				h.env["FAKE_FAIL_LATEST"] = "true"
				if scenario == "release-list-failed" {
					h.env["FAKE_FAIL_RELEASE_LIST"] = "true"
				}
			}
			previousLatest := string(h.read(t, "latest"))
			output, publishErr := h.command("bash", "-c", publish)
			failed := scenario == "existing-fixed" || scenario == "existing-fixed-tag" || scenario == "fixed-upload-failed" || scenario == "fixed-promotion-failed" || scenario == "latest-query-failed" || scenario == "release-list-failed" || scenario == "first-release-list-failed"
			if (publishErr != nil) != failed {
				t.Fatalf("publication outcome %v: %s", publishErr, output)
			}
			// Reproduce the validated workflow success/output condition using its actual bash bodies.
			if publishErr == nil && strings.Contains(string(h.read(t, "output")), "promoted=true\n") {
				if output, err := h.command("bash", "-c", syncRun); err != nil {
					t.Fatalf("sync %v: %s", err, output)
				}
			}
			devMutated := false
			for _, call := range h.calls(t) {
				if call[1] != "view" && call[2] == "dev" {
					devMutated = true
				}
				if call[1] != "view" && call[2] != "dev" && containsSyncArg(call, "--clobber") {
					t.Fatalf("fixed release overwritten: %q", call)
				}
				if (scenario == "existing-fixed" || scenario == "existing-fixed-tag") && call[1] != "view" {
					t.Fatalf("existing fixed identity mutated: %q", call)
				}
			}
			if devMutated != (scenario == "promoted" || scenario == "first-published-release") {
				t.Fatalf("unexpected alias sync: %t", devMutated)
			}
			if failed && string(h.read(t, "latest")) != previousLatest {
				t.Fatal("failed publication changed the live latest")
			}
			if scenario == "promoted" || scenario == "first-published-release" {
				h.assertDev(t, syncCommit)
			}
			if scenario == "old-slow-build" && string(h.read(t, "output")) != "promoted=false\n" {
				t.Fatal("old build emitted promotion")
			}
		})
	}
}

// The fake models persisted assets/notes/tags, not just matching command strings.
const fakeReleaseGH = `#!/usr/bin/env bash
set -euo pipefail
jq -cn --args '$ARGS.positional' -- "$@" >> "$FAKE_ROOT/calls.jsonl"
[[ "$1" == release ]] || exit 90
action=$2
shift 2
tag=""
if [[ $# -gt 0 && "$1" != --* ]]; then tag=$1; shift; fi
latest=""; draft=""; prerelease=""; target=""; notes=""; has_notes=false; raw=false; clobber=false
files=()
while [[ $# -gt 0 ]]; do
  case "$1" in
    --repo|--title|--json|--limit) shift 2 ;;
    --exclude-drafts|--exclude-pre-releases) shift ;;
    --jq) raw=true; shift 2 ;;
    --target) target=$2; shift 2 ;;
    --notes) notes=$2; has_notes=true; shift 2 ;;
    --latest=*) latest=${1#*=}; shift ;;
    --draft) draft=true; shift ;;
    --draft=*) draft=${1#*=}; shift ;;
    --prerelease) prerelease=true; shift ;;
    --prerelease=*) prerelease=${1#*=}; shift ;;
    --generate-notes) shift ;;
    --clobber) clobber=true; shift ;;
    --*) exit 91 ;;
    *) files+=("$1"); shift ;;
  esac
done
if [[ "$action" == list && -z "$tag" ]]; then
  [[ "${FAKE_FAIL_RELEASE_LIST:-}" != true ]] || exit 26
  printf '%s\n' "$(< "$FAKE_ROOT/latest")"
  exit 0
fi
if [[ "$action" == view && -z "$tag" ]]; then
  [[ "${FAKE_FAIL_LATEST:-}" != true && -s "$FAKE_ROOT/latest" ]] || exit 18
  if [[ "$raw" == true ]]; then printf '%s\n' "$(< "$FAKE_ROOT/latest")"; else jq -cn --arg tag "$(< "$FAKE_ROOT/latest")" '{tagName:$tag}'; fi
  exit 0
fi
if [[ "$tag" == dev ]]; then key=dev; elif [[ "$tag" == "$VERSION" ]]; then key=fixed; else exit 92; fi
case "$action" in
  view)
    [[ -f "$FAKE_ROOT/$key.exists" ]] || exit 1
    if [[ "$key" == fixed ]]; then printf '%s\n' "$(< "$FAKE_ROOT/fixed.json")"; fi
    ;;
  create)
    [[ ! -f "$FAKE_ROOT/$key.exists" ]] || exit 19
    : > "$FAKE_ROOT/$key.exists"
    if [[ "$key" == fixed ]]; then
      jq '.isDraft=true | .assets=[]' "$FAKE_ROOT/fixed.json" > "$FAKE_ROOT/fixed.tmp"
      mv "$FAKE_ROOT/fixed.tmp" "$FAKE_ROOT/fixed.json"
    elif [[ ! -f "$FAKE_ROOT/dev.tag" ]]; then printf '%s' "$target" > "$FAKE_ROOT/dev.tag"; fi
    ;&
  edit)
    [[ "${FAKE_FAIL_EDIT:-}" != "$tag" ]] || exit 20
    [[ -f "$FAKE_ROOT/$key.exists" ]] || exit 21
    [[ -z "$draft" ]] || printf '%s' "$draft" > "$FAKE_ROOT/$key.draft"
    [[ -z "$prerelease" ]] || printf '%s' "$prerelease" > "$FAKE_ROOT/$key.prerelease"
    [[ "$has_notes" == false ]] || printf '%s' "$notes" > "$FAKE_ROOT/$key.notes"
    if [[ "$key" == fixed && "$draft" == false ]]; then
      jq '.isDraft=false' "$FAKE_ROOT/fixed.json" > "$FAKE_ROOT/fixed.tmp"
      mv "$FAKE_ROOT/fixed.tmp" "$FAKE_ROOT/fixed.json"
    fi
    if [[ "$latest" == true && "${FAKE_IGNORE_PROMOTION:-}" != true ]]; then printf '%s' "$tag" > "$FAKE_ROOT/latest"; fi
    ;;
  upload)
    [[ -f "$FAKE_ROOT/$key.exists" && ${#files[@]} == 5 ]] || exit 22
    mkdir -p "$FAKE_ROOT/$key-assets"
    for file in "${files[@]}"; do
      name=${file##*/}
      case "$name" in mi-node-linux-amd64|mi-node-linux-arm64|xbctl-linux-amd64|xbctl-linux-arm64|node-release.json) ;; *) exit 23 ;; esac
      [[ ! -f "$FAKE_ROOT/$key-assets/$name" || "$clobber" == true ]] || exit 24
      cp -- "$file" "$FAKE_ROOT/$key-assets/$name"
      [[ "${FAKE_FAIL_UPLOAD:-}" != "$tag" ]] || exit 25
    done
    if [[ "$key" == fixed ]]; then
      jq --slurpfile source "$FAKE_ROOT/source.json" '.assets=$source[0].assets' "$FAKE_ROOT/fixed.json" > "$FAKE_ROOT/fixed.tmp"
      mv "$FAKE_ROOT/fixed.tmp" "$FAKE_ROOT/fixed.json"
    fi
    ;;
  *) exit 93 ;;
esac
`
