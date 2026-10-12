package updateagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/discovery"
)

const enrollmentStatusSecret = "injected-token-error-request-fixture"

func requireEnrollmentStatus(t *testing.T, directory, state, stage, code string) *discovery.EnrollmentStatus {
	t.Helper()
	status := discovery.ReadEnrollmentStatus(directory)
	if status == nil || status.State != state || status.Stage != stage || status.Code != code || !status.Valid() {
		t.Fatalf("status: %+v want %s/%s/%s", status, state, stage, code)
	}
	path := filepath.Join(directory, discovery.EnrollmentStatusFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil || len(fields) != 4 {
		t.Fatalf("status schema: %s %v", data, err)
	}
	for _, secret := range []string{enrollmentStatusSecret, "server-fixture", "agent_token", "fixture-machine-identity", "/fixture/config.yml"} {
		if bytes.Contains(data, []byte(secret)) {
			t.Fatal("private error/request/config escaped status")
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("status mode: %v", info.Mode())
	}
	return status
}

func statusAutoFixture(t *testing.T, authority string) autoInstaller {
	i := autoFixture(t, authority)
	i.checkLayout = func(context.Context) error { return nil }
	i.lock = func() (func(), error) { return func() {}, nil }
	i.installUnits = func(context.Context) error { return nil }
	i.finishBootstrap = func(context.Context) error { return nil }
	i.run = func(context.Context, Config) error { return nil }
	return i
}

func TestEnrollmentStatusLayoutAndLockFailures(t *testing.T) {
	for _, tc := range []struct {
		name, stage, code string
		err               error
	}{
		{"layout", "layout", "shared_binary_layout", errors.New("shared_binary_layout")},
		{"systemctl", "layout", "systemd_failed", &exec.ExitError{Stderr: []byte(enrollmentStatusSecret)}},
		{"lock", "lock", "update_busy", errors.New(enrollmentStatusSecret)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := autoInstaller{directory: t.TempDir(), checkLayout: func(context.Context) error { return nil }}
			if tc.stage == "layout" {
				i.checkLayout = func(context.Context) error { return tc.err }
			}
			i.lock = func() (func(), error) { return nil, tc.err }
			if err := i.install(context.Background(), "/fixture/config.yml"); err == nil {
				t.Fatal("failure swallowed")
			}
			requireEnrollmentStatus(t, i.directory, "retrying", tc.stage, tc.code)
		})
	}
}

func TestEnrollmentStatusBootstrapFailuresAndScheduling(t *testing.T) {
	for _, command := range []string{"daemon-reload", "enable", "start"} {
		t.Run(command, func(t *testing.T) {
			clearBootstrapTestEnvironment(t)
			configPath := filepath.Join(t.TempDir(), "config.yml")
			writeBootstrapTestConfig(t, configPath, "panel: {url: https://panel.example, token: fixture, node_id: 7}\n")
			directory := t.TempDir()
			err := bootstrapUnits(context.Background(), t.TempDir(), configPath, directory, func(_ context.Context, args ...string) (string, error) {
				if args[0] == command {
					return "", errors.New(enrollmentStatusSecret)
				}
				return "", nil
			})
			if err == nil || err.Error() != enrollmentStatusSecret {
				t.Fatal("original systemctl error changed")
			}
			requireEnrollmentStatus(t, directory, "retrying", "bootstrap", "systemd_failed")
		})
	}
	t.Run("config", func(t *testing.T) {
		clearBootstrapTestEnvironment(t)
		directory := t.TempDir()
		if err := bootstrapUnits(context.Background(), t.TempDir(), filepath.Join(t.TempDir(), "missing.yml"), directory, func(context.Context, ...string) (string, error) { t.Fatal("invalid config scheduled"); return "", nil }); err == nil {
			t.Fatal("invalid config accepted")
		}
		requireEnrollmentStatus(t, directory, "retrying", "config", "config_invalid")
	})
	t.Run("registered-before-start-returns", func(t *testing.T) {
		clearBootstrapTestEnvironment(t)
		configPath := filepath.Join(t.TempDir(), "config.yml")
		writeBootstrapTestConfig(t, configPath, "panel: {url: https://panel.example, token: fixture, node_id: 7}\n")
		directory := t.TempDir()
		if err := bootstrapUnits(context.Background(), t.TempDir(), configPath, directory, func(_ context.Context, args ...string) (string, error) {
			if args[0] == "enable" {
				requireEnrollmentStatus(t, directory, "starting", "bootstrap", "")
			}
			if args[0] == "start" {
				status := newEnrollmentAttempt(directory, "units")
				status.state = "registered"
				status.finish(nil)
			}
			return "", nil
		}); err != nil {
			t.Fatal(err)
		}
		requireEnrollmentStatus(t, directory, "registered", "units", "")
	})
}

type enrollmentStatusTransport struct{}

func (enrollmentStatusTransport) RoundTrip(*http.Request) (*http.Response, error) {
	return nil, errors.New(enrollmentStatusSecret)
}

func TestEnrollmentStatusAPIFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body, code string
		transport  bool
	}{
		{name: "401", status: 401, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "invalid_credentials"},
		{name: "409", status: 409, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "scope_conflict"},
		{name: "scope-mismatch", status: 409, body: `{"error":{"code":"scope_mismatch"}}`, code: "scope_mismatch"},
		{name: "422", status: 422, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "validation_failed"},
		{name: "429", status: 429, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "rate_limited"},
		{name: "503", status: 503, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "api_unavailable"},
		{name: "json", status: 200, body: "{", code: "invalid_api_response"},
		{name: "response", status: 200, body: `{"data":{}}`, code: "invalid_api_response"},
		{name: "types", status: 200, body: `{"data":{"installation_id":123}}`, code: "invalid_api_response"},
		{name: "transport", code: "transport_failed", transport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := statusAutoFixture(t, "https://fixture.example")
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			i.loadRoot = func(string) (*config.RootConfig, error) {
				return &config.RootConfig{Config: *legacyBinding(server.URL, "server-fixture", 2, 9)}, nil
			}
			i.client = func(authority, token string) (*Client, error) {
				client, err := NewClient(authority, token)
				if err == nil {
					client.HTTP = server.Client()
					if tc.transport {
						client.HTTP = &http.Client{Transport: enrollmentStatusTransport{}}
					}
				}
				return client, err
			}
			if err := i.install(context.Background(), "/fixture/config.yml"); err == nil {
				t.Fatal("enrollment failure swallowed")
			}
			requireEnrollmentStatus(t, i.directory, "retrying", "enroll", tc.code)
			c, err := loadConfigAt(i.directory, i.identity)
			if err != nil || c.Enrolled || len(c.Token) != 43 {
				t.Fatal("failure lost pending identity")
			}
			before, err := os.ReadFile(filepath.Join(i.directory, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if err := i.install(context.Background(), "/fixture/config.yml"); err == nil {
				t.Fatal("failed retry succeeded")
			}
			after, _ := os.ReadFile(filepath.Join(i.directory, "config.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("retry changed identity/token/scope")
			}
		})
	}
}

func TestEnrollmentStatusConfigAndAuthorityFailures(t *testing.T) {
	for _, tc := range []struct{ name, stage, code string }{
		{"config", "config", "config_invalid"},
		{"credential", "authority", "credential_missing"},
		{"authority", "authority", "authority_unavailable"},
		{"scope-conflict", "authority", "scope_conflict"},
		{"scope-mismatch", "authority", "scope_mismatch"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			i := statusAutoFixture(t, "https://original.example")
			switch tc.name {
			case "config":
				i.loadRoot = func(string) (*config.RootConfig, error) { return nil, errors.New(enrollmentStatusSecret) }
			case "credential":
				i.loadRoot = func(string) (*config.RootConfig, error) {
					return &config.RootConfig{Config: *legacyBinding("https://original.example", "", 2, 9)}, nil
				}
			case "authority":
				i.loadRoot = func(string) (*config.RootConfig, error) {
					return &config.RootConfig{Config: *legacyBinding("ftp://original.example", "server-fixture", 2, 9)}, nil
				}
			case "scope-conflict":
				i.loadRoot = func(string) (*config.RootConfig, error) {
					return &config.RootConfig{Instances: []config.Config{*legacyBinding("https://original.example", "server-fixture", 2), *legacyBinding("https://original.example", "different-fixture", 9)}}, nil
				}
			case "scope-mismatch":
				c := savedAutoFixture(t, i)
				c.Enrolled, c.AutoEnroll, c.EnrollmentID, c.AutoScope = false, true, "", json.RawMessage(`{"kind":"legacy","node_ids":[99]}`)
				if err := AtomicJSON(filepath.Join(i.directory, "config.json"), c); err != nil {
					t.Fatal(err)
				}
			}
			i.client = func(string, string) (*Client, error) { t.Fatal("invalid bindings reached API"); return nil, nil }
			if err := i.install(context.Background(), "/fixture/config.yml"); err == nil {
				t.Fatal("invalid bindings accepted")
			}
			requireEnrollmentStatus(t, i.directory, "retrying", tc.stage, tc.code)
		})
	}
}

func TestEnrollmentStatusRegisteredDisabledUnitsAndPoll(t *testing.T) {
	for _, mode := range []string{"registered", "disabled", "units", "poll", "task-error", "status-write-failed"} {
		t.Run(mode, func(t *testing.T) {
			i := statusAutoFixture(t, "https://original.example")
			c := savedAutoFixture(t, i)
			if mode == "disabled" {
				c.Disabled = true
				if err := AtomicJSON(filepath.Join(i.directory, "config.json"), c); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "status-write-failed" {
				if err := os.Mkdir(filepath.Join(i.directory, discovery.EnrollmentStatusFile), 0700); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(filepath.Join(i.directory, "config.json"))
			locked, ran := false, false
			i.lock = func() (func(), error) { locked = true; return func() { locked = false }, nil }
			i.installUnits = func(context.Context) error {
				if mode == "disabled" {
					t.Fatal("disabled installation started units")
				}
				if mode == "units" {
					return errors.New(enrollmentStatusSecret)
				}
				return nil
			}
			i.run = func(_ context.Context, got Config) error {
				ran = true
				if locked || !reflect.DeepEqual(got, c) {
					t.Fatal("run ownership or config changed")
				}
				if mode != "status-write-failed" {
					requireEnrollmentStatus(t, i.directory, "registered", "units", "")
				}
				if mode == "registered" || mode == "poll" || mode == "task-error" {
					agent := &Agent{Config: got, Engine: &Engine{Dir: i.directory}}
					var pollError error
					if mode == "poll" {
						pollError = &APIError{Status: 401, Code: enrollmentStatusSecret}
					}
					agent.publishPollStatus("", pollError)
					if mode == "task-error" {
						return &APIError{Status: 401, Code: enrollmentStatusSecret}
					}
					return pollError
				}
				return nil
			}
			err := i.install(context.Background(), "/fixture/config.yml")
			failed := mode == "units" || mode == "poll" || mode == "task-error"
			if (err != nil) != failed || locked || (ran != (mode != "disabled" && mode != "units")) {
				t.Fatalf("installation outcome: mode=%s err=%v", mode, err)
			}
			after, _ := os.ReadFile(filepath.Join(i.directory, "config.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("existing registered/disabled identity changed")
			}
			switch mode {
			case "registered", "task-error":
				requireEnrollmentStatus(t, i.directory, "registered", "poll", "")
			case "disabled":
				requireEnrollmentStatus(t, i.directory, "disabled", "units", "local_disabled")
			case "units":
				requireEnrollmentStatus(t, i.directory, "retrying", "units", "systemd_failed")
			case "poll":
				requireEnrollmentStatus(t, i.directory, "retrying", "poll", "invalid_credentials")
			case "status-write-failed":
				if discovery.ReadEnrollmentStatus(i.directory) != nil {
					t.Fatal("invalid status path accepted")
				}
			}
		})
	}
}

func TestEnrollmentStatusNonOwnerPreservesTerminal(t *testing.T) {
	for _, state := range []string{"registered", "disabled"} {
		t.Run(state, func(t *testing.T) {
			directory := t.TempDir()
			previous := newEnrollmentAttempt(directory, "units")
			previous.state = state
			if state == "disabled" {
				previous.code = "local_disabled"
			}
			previous.finish(nil)
			before, _ := os.ReadFile(filepath.Join(directory, discovery.EnrollmentStatusFile))
			i := autoInstaller{directory: directory, checkLayout: func(context.Context) error { return nil }, lock: func() (func(), error) { return nil, errors.New(enrollmentStatusSecret) }}
			if err := i.install(context.Background(), "/fixture/config.yml"); err == nil {
				t.Fatal("busy accepted")
			}
			unsupported := newEnrollmentAttempt(directory, "bootstrap")
			unsupported.state, unsupported.code, unsupported.preserveTerminal = "unsupported", "unsupported_platform", true
			unsupported.finish(nil)
			after, _ := os.ReadFile(filepath.Join(directory, discovery.EnrollmentStatusFile))
			if !bytes.Equal(before, after) {
				t.Fatal("non-owner overwrote terminal status")
			}
		})
	}
}

func TestEnrollmentStatusUnsupportedAndUnknownAreSanitized(t *testing.T) {
	directory := t.TempDir()
	unsupported := newEnrollmentAttempt(directory, "bootstrap")
	unsupported.state, unsupported.code = "unsupported", "unsupported_platform"
	unsupported.finish(nil)
	requireEnrollmentStatus(t, directory, "unsupported", "bootstrap", "unsupported_platform")
	unknown := newEnrollmentAttempt(directory, "poll")
	unknown.finish(errors.New(enrollmentStatusSecret))
	requireEnrollmentStatus(t, directory, "retrying", "poll", "unknown_error")
	// Writing diagnostics must leave existing transaction/backoff files alone.
	names, err := os.ReadDir(directory)
	if err != nil || len(names) != 1 || strings.HasPrefix(names[0].Name(), ".update-") {
		t.Fatalf("status publication leaked artifacts: %v %v", names, err)
	}
}
