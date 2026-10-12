package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func noEnv(string) string { return "" }

func writeArtifacts(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	for _, arch := range []string{"amd64", "arm64"} {
		for _, component := range []string{"mi-node", "xbctl"} {
			name := component + "-linux-" + arch
			data := append([]byte(name), 0, 0xff, '\n')
			if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
				t.Fatal(err)
			}
			files[name] = data
		}
	}
	return files
}

func TestManifestExactContractAndRealFileMetadata(t *testing.T) {
	dir := t.TempDir()
	files := writeArtifacts(t, dir)
	output := filepath.Join(dir, "node-release.json")
	var stdout bytes.Buffer
	args := []string{"--version", "v20260403.123456.2", "--repository", "owner/repo", "--dir", dir, "--output", output}
	if err := run(args, noEnv, time.Time{}, &stdout); err != nil {
		t.Fatal(err)
	}
	if stdout.Len() != 0 {
		t.Fatal("unexpected non-JSON status output")
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	if len(raw) != 4 || raw["version"] == nil || raw["os"] == nil || raw["min_agent_protocol"] == nil || raw["artifacts"] == nil {
		t.Fatalf("wrong top-level contract: %s", data)
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	if m.Version != "v20260403.123456.2" || m.OS != "linux" || m.MinProtocol != 1 || len(m.Artifacts) != 4 {
		t.Fatalf("wrong manifest: %+v", m)
	}
	var rawArtifacts []map[string]json.RawMessage
	if err := json.Unmarshal(raw["artifacts"], &rawArtifacts); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for i, a := range m.Artifacts {
		fields := rawArtifacts[i]
		if len(fields) != 5 || fields["arch"] == nil || fields["component"] == nil || fields["https_url"] == nil || fields["sha256"] == nil || fields["size_bytes"] == nil {
			t.Fatalf("wrong artifact fields: %+v", fields)
		}
		name := a.Component + "-linux-" + a.Arch
		payload, ok := files[name]
		if !ok || seen[name] {
			t.Fatalf("unexpected or duplicate artifact: %s", name)
		}
		seen[name] = true
		expectedHash := fmt.Sprintf("%x", sha256.Sum256(payload))
		expectedURL := "https://github.com/owner/repo/releases/download/" + m.Version + "/" + name
		if a.SHA256 != expectedHash || a.Size != int64(len(payload)) || a.URL != expectedURL {
			t.Fatalf("metadata does not match real bytes: %+v", a)
		}
	}
	// Recompute metadata after a real byte/size change; never reuse stale checksums.
	name := "mi-node-linux-arm64"
	changed := []byte("a changed artifact with a different size")
	if err := os.WriteFile(filepath.Join(dir, name), changed, 0644); err != nil {
		t.Fatal(err)
	}
	args[len(args)-1] = "-"
	if err := run(args, noEnv, time.Time{}, &stdout); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(stdout.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	for _, a := range m.Artifacts {
		if a.Component+"-linux-"+a.Arch == name && (a.Size != int64(len(changed)) || a.SHA256 != fmt.Sprintf("%x", sha256.Sum256(changed))) {
			t.Fatalf("stale metadata: %+v", a)
		}
	}
}

func TestManifestFailsForEveryMissingOrEmptyArtifact(t *testing.T) {
	for _, name := range []string{"mi-node-linux-amd64", "mi-node-linux-arm64", "xbctl-linux-amd64", "xbctl-linux-arm64"} {
		for _, empty := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/empty=%t", name, empty), func(t *testing.T) {
				dir := t.TempDir()
				writeArtifacts(t, dir)
				path := filepath.Join(dir, name)
				var err error
				if empty {
					err = os.Truncate(path, 0)
				} else {
					err = os.Remove(path)
				}
				if err != nil {
					t.Fatal(err)
				}
				output := filepath.Join(dir, "node-release.json")
				original := []byte("previous manifest")
				if err := os.WriteFile(output, original, 0644); err != nil {
					t.Fatal(err)
				}
				var stdout bytes.Buffer
				args := []string{"--version", "v1.2.3", "--repository", "owner/repo", "--dir", dir, "--output", output}
				if err := run(args, noEnv, time.Time{}, &stdout); err == nil || !strings.Contains(err.Error(), name) {
					t.Fatalf("bad error: %v", err)
				}
				got, err := os.ReadFile(output)
				if err != nil || !bytes.Equal(got, original) || stdout.Len() != 0 {
					t.Fatalf("failure replaced manifest: %q %v", got, err)
				}
			})
		}
	}
}

func TestReleaseVersionsStrictUTCAndRerunIdentity(t *testing.T) {
	now := time.Date(2026, 4, 4, 1, 0, 0, 123, time.FixedZone("UTC+8", 8*3600))
	env := map[string]string{"GITHUB_RUN_ID": "123456", "GITHUB_RUN_ATTEMPT": "1"}
	getenv := func(key string) string { return env[key] }
	first, err := releaseVersion("", getenv, now)
	if err != nil || first != "v20260403.123456.1" {
		t.Fatalf("wrong UTC version: %s %v", first, err)
	}
	env["GITHUB_RUN_ATTEMPT"] = "2"
	second, err := releaseVersion("", getenv, now)
	if err != nil || second != "v20260403.123456.2" || first == second {
		t.Fatalf("rerun reused version: %s %v", second, err)
	}
	local, err := releaseVersion("", noEnv, now)
	if err != nil || local != fmt.Sprintf("v20260403.%d.0", now.UnixNano()) {
		t.Fatalf("wrong local version: %s %v", local, err)
	}
	later, err := releaseVersion("", noEnv, now.Add(time.Nanosecond))
	if err != nil || local == later {
		t.Fatal("local automatic versions collided")
	}
	for _, valid := range []string{"v0.0.0", "v1.2.3", "v20260403.123456.2"} {
		if got, err := releaseVersion(valid, noEnv, now); err != nil || got != valid {
			t.Fatalf("valid version rejected: %s %v", got, err)
		}
	}
	for _, invalid := range []string{"dev", "1.2.3", "v01.2.3", "v1.02.3", "v1.2.03", "v1x2x3", "v1.2.3-rc1", "v1.2.3+build", "v1.2.3\n"} {
		if _, err := releaseVersion(invalid, noEnv, now); err == nil {
			t.Fatalf("invalid version accepted: %q", invalid)
		}
	}
	for _, invalid := range []map[string]string{{"GITHUB_RUN_ID": "01", "GITHUB_RUN_ATTEMPT": "1"}, {"GITHUB_RUN_ID": "123"}, {"GITHUB_RUN_ATTEMPT": "2"}} {
		if _, err := releaseVersion("", func(k string) string { return invalid[k] }, now); err == nil {
			t.Fatalf("invalid CI identity accepted: %+v", invalid)
		}
	}
	var stdout bytes.Buffer
	if err := run([]string{"--version-only"}, getenv, now, &stdout); err != nil || stdout.String() != second+"\n" {
		t.Fatalf("version-only failed: %q %v", stdout.String(), err)
	}
}

func TestWorkflowYAMLAndReleaseGates(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", ".github", "workflows", "ci.yml"))
	if err != nil {
		t.Fatal(err)
	}
	var workflow map[string]any
	if err := yaml.Unmarshal(data, &workflow); err != nil {
		t.Fatal(err)
	}
	on := workflow["on"].(map[string]any)
	push := on["push"].(map[string]any)
	if !reflect.DeepEqual(push["branches"], []any{"main", "master", "dev"}) {
		t.Fatalf("missing branch triggers: %+v", push)
	}
	jobs := workflow["jobs"].(map[string]any)
	release := jobs["release"].(map[string]any)
	build := jobs["build"].(map[string]any)
	if !reflect.DeepEqual(release["needs"], []any{"test", "version", "build"}) || !reflect.DeepEqual(build["needs"], []any{"test", "version"}) {
		t.Fatal("release is not gated by tests and versioned builds")
	}
	if release["if"] != "github.event_name == 'push' || github.event_name == 'workflow_dispatch'" {
		t.Fatal("release must cover all push branches and exclude PRs")
	}
	concurrency := release["concurrency"].(map[string]any)
	if concurrency["cancel-in-progress"] != false || concurrency["group"] != "mi-node-release" {
		t.Fatal("publication must serialize latest updates across all versions")
	}
	if strings.Contains(string(data), "git tag -f dev") || strings.Contains(string(data), "--clobber") {
		t.Fatal("immutable releases must never overwrite old assets/tags")
	}
}
