package updateagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func agentStatusFixture(t *testing.T, server *httptest.Server) *Agent {
	t.Helper()
	a := contractAgent(t, t.TempDir(), server)
	a.Config = Config{InstallationID: UUID(), Authority: server.URL, Token: strings.Repeat("A", 43), EnrollmentID: UUID(), Enrolled: true, Scope: json.RawMessage(`{"kind":"legacy","node_ids":[7]}`), MachineIdentity: "fixture-machine-identity"}
	a.Client.Token = a.Config.Token
	if err := AtomicJSON(filepath.Join(a.Engine.Dir, "config.json"), a.Config); err != nil {
		t.Fatal(err)
	}
	for _, path := range a.Engine.Paths {
		if err := os.WriteFile(path, []byte("non-executable fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return a
}

func expireAgentStatusBackoff(t *testing.T, a *Agent) {
	t.Helper()
	if err := AtomicJSON(filepath.Join(a.Engine.Dir, "retry.json"), retryState{Failures: 1, Next: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRunPollStatusRecoversRegistered(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Agent.Run uses Linux flock")
	}
	for _, tc := range []struct {
		name, body, code string
		status           int
		transport        bool
	}{
		{name: "401", status: 401, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "invalid_credentials"},
		{name: "403", status: 403, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "invalid_credentials"},
		{name: "409", status: 409, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "scope_conflict"},
		{name: "scope-mismatch", status: 409, body: `{"error":{"code":"scope_mismatch"}}`, code: "scope_mismatch"},
		{name: "422", status: 422, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "validation_failed"},
		{name: "429", status: 429, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "rate_limited"},
		{name: "503", status: 503, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "api_unavailable"},
		{name: "unknown-api-code", status: 404, body: `{"error":{"code":"injected-token-error-request-fixture"}}`, code: "unknown_error"},
		{name: "json", status: 200, body: "{", code: "invalid_api_response"},
		{name: "types", status: 200, body: `{"data":{"candidate":123}}`, code: "invalid_api_response"},
		{name: "transport", code: "transport_failed", transport: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var polls atomic.Int32
			var recovered atomic.Bool
			requests := make(chan string, 2)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != endpoint+"/poll" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				var body struct {
					InstallationID string `json:"installation_id"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				requests <- body.InstallationID
				polls.Add(1)
				if !recovered.Load() {
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
					return
				}
				_, _ = w.Write([]byte(`{"data":{"candidate":null,"global_enabled":true,"policy":{"enabled":true,"revision":1}}}`))
			}))
			defer server.Close()
			a := agentStatusFixture(t, server)
			before, err := os.ReadFile(filepath.Join(a.Engine.Dir, "config.json"))
			if err != nil {
				t.Fatal(err)
			}
			if tc.transport {
				a.Client.HTTP = &http.Client{Transport: enrollmentStatusTransport{}}
			}
			if err := a.Run(context.Background(), "", "systemd", false); err == nil {
				t.Fatal("poll failure swallowed")
			}
			requireEnrollmentStatus(t, a.Engine.Dir, "retrying", "poll", tc.code)
			stored, err := os.ReadFile(filepath.Join(a.Engine.Dir, "config.json"))
			if err != nil || !bytes.Equal(before, stored) || !a.Config.Enrolled {
				t.Fatal("poll failure changed registered identity")
			}
			config := a.Config
			fresh := contractAgent(t, a.Engine.Dir, server)
			fresh.Config, fresh.Client.Token = config, config.Token
			recovered.Store(true)
			if err := fresh.Run(context.Background(), "", "systemd", false); err != nil {
				t.Fatal(err)
			}
			// Backoff is a skipped poll, not evidence of recovery.
			requireEnrollmentStatus(t, a.Engine.Dir, "retrying", "poll", tc.code)
			expireAgentStatusBackoff(t, fresh)
			if err := fresh.Run(context.Background(), "", "systemd", false); err != nil {
				t.Fatal(err)
			}
			requireEnrollmentStatus(t, fresh.Engine.Dir, "registered", "poll", "")
			wantPolls := int32(2)
			if tc.transport {
				wantPolls = 1
			}
			if polls.Load() != wantPolls {
				t.Fatalf("poll count: %d want %d", polls.Load(), wantPolls)
			}
			for range wantPolls {
				if id := <-requests; id != config.InstallationID {
					t.Fatal("recovery polled with a different installation")
				}
			}
			after, err := os.ReadFile(filepath.Join(a.Engine.Dir, "config.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("poll recovery changed config")
			}
			if _, err := os.Stat(fresh.Engine.journalPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("empty candidate created a transaction: %v", err)
			}
		})
	}
}

func TestAgentPollStatusPreservesLocalGatesAndPendingEnrollment(t *testing.T) {
	for _, tc := range []struct {
		name, capability, state, code string
		disabled, pending             bool
	}{
		{name: "disabled", disabled: true, state: "disabled", code: "local_disabled"},
		{name: "unsupported", capability: "unsupported_init", state: "unsupported", code: "unsupported_platform"},
		{name: "layout", capability: "shared_binary_layout", state: "unsupported", code: "shared_binary_layout"},
		{name: "unknown-capability", capability: enrollmentStatusSecret, state: "unsupported", code: "unsupported_platform"},
		{name: "pending", pending: true, state: "starting"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			directory := t.TempDir()
			a := &Agent{Config: Config{Enrolled: !tc.pending, Disabled: tc.disabled}, Engine: &Engine{Dir: directory}}
			if tc.pending {
				newEnrollmentAttempt(directory, "enroll").finish(nil)
			}
			for _, err := range []error{&APIError{Status: 401, Code: enrollmentStatusSecret}, nil} {
				a.publishPollStatus(tc.capability, err)
				stage := "poll"
				if tc.pending {
					stage = "enroll"
				}
				requireEnrollmentStatus(t, directory, tc.state, stage, tc.code)
				if a.Config.Enrolled == tc.pending || a.Config.Disabled != tc.disabled {
					t.Fatal("diagnostics changed enrollment facts")
				}
			}
		})
	}
}

func TestAgentRunTaskAndLockErrorsPreservePollStatus(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Agent.Run uses Linux flock")
	}
	for _, mode := range []string{"claim", "invalid-candidate", "lock", "event-replay"} {
		t.Run(mode, func(t *testing.T) {
			var polls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == endpoint+"/poll" {
					polls.Add(1)
					candidate := Candidate{TaskID: UUID(), ReleaseID: UUID(), Version: "v1.2.3", Revision: 1}
					if mode == "invalid-candidate" {
						candidate.TaskID = "invalid"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"candidate": candidate, "global_enabled": true, "policy": map[string]any{"enabled": true, "revision": 1}}})
					return
				}
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":{"code":"injected-token-error-request-fixture"}}`))
			}))
			defer server.Close()
			a := agentStatusFixture(t, server)
			previous := newEnrollmentAttempt(a.Engine.Dir, "poll")
			previous.state = "registered"
			if mode == "claim" || mode == "invalid-candidate" {
				previous.finish(&APIError{Status: 503})
			} else {
				previous.finish(nil)
			}
			if mode == "lock" {
				unlock, err := Lock(a.Engine.Paths[0])
				if err != nil {
					t.Fatal(err)
				}
				defer unlock()
			}
			if mode == "event-replay" {
				tx := &Transaction{TaskID: UUID(), Resolved: true, Phase: "failed", Pending: []Event{{Seq: 1, State: "failed"}}}
				if err := a.Engine.Save(tx); err != nil {
					t.Fatal(err)
				}
			}
			if err := a.Run(context.Background(), "", "systemd", false); err == nil {
				t.Fatal("non-poll failure swallowed")
			}
			requireEnrollmentStatus(t, a.Engine.Dir, "registered", "poll", "")
			wantPolls := int32(1)
			if mode == "lock" || mode == "event-replay" {
				wantPolls = 0
			}
			if polls.Load() != wantPolls {
				t.Fatalf("poll count: %d want %d", polls.Load(), wantPolls)
			}
		})
	}
}
