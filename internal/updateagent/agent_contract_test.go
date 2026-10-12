package updateagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func contractAgent(t *testing.T, dir string, srv *httptest.Server) *Agent {
	t.Helper()
	client, err := NewClient(srv.URL, "fixture-token")
	if err != nil {
		t.Fatal(err)
	}
	client.HTTP.Transport = srv.Client().Transport
	return &Agent{Config: Config{InstallationID: "fixture-installation"}, Client: client, Engine: &Engine{Dir: dir, Paths: [2]string{filepath.Join(dir, "missing-node"), filepath.Join(dir, "missing-cli")}}}
}
func contractDropACK(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	conn, _, err := w.(http.Hijacker).Hijack()
	if err != nil {
		t.Error(err)
		return
	}
	_ = conn.Close()
}
func TestContractAgentLostEventACKReplaysSamePayloadAndSeq(t *testing.T) {
	requests := make(chan []byte, 4)
	count := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" || r.URL.Path != endpoint+"/tasks/task/events" || r.Header.Get("Authorization") != "Bearer fixture-token" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		requests <- b
		count++
		if count == 1 {
			contractDropACK(t, w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"accepted_seq": 1, "state": "installing", "lease_expires_at": time.Now().Add(time.Hour)}})
	}))
	defer srv.Close()
	dir := t.TempDir()
	a := contractAgent(t, dir, srv)
	tx := &Transaction{TaskID: "task", ClaimID: UUID(), Claim: Claim{AttemptID: UUID(), LeaseToken: "fixture-lease"}}
	if err := a.Queue(tx, "installing", ""); err == nil {
		t.Fatal("lost ACK reported success")
	}
	if tx.InstallingACK || tx.AckSeq != 0 || tx.LastSeq != 1 || len(tx.Pending) != 1 {
		t.Fatalf("bad pending state %+v", tx)
	}
	fresh := contractAgent(t, dir, srv)
	recovered, err := fresh.Engine.Load()
	if err != nil {
		t.Fatal(err)
	}
	if recovered.LastSeq != 1 || len(recovered.Pending) != 1 {
		t.Fatalf("pending not durable %+v", recovered)
	}
	if err = fresh.Flush(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	first, second := <-requests, <-requests
	if !bytes.Equal(first, second) {
		t.Fatalf("retry payload changed: %s versus %s", first, second)
	}
	var ev Event
	if err = json.Unmarshal(second, &ev); err != nil {
		t.Fatal(err)
	}
	if ev.Seq != 1 || ev.State != "installing" {
		t.Fatalf("wrong event %+v", ev)
	}
	durable, err := fresh.Engine.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !durable.InstallingACK || durable.AckSeq != 1 || durable.LastSeq != 1 || len(durable.Pending) != 0 {
		t.Fatalf("ACK not durable %+v", durable)
	}
	if err = fresh.Flush(context.Background(), durable); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 0 {
		t.Fatal("ACKed event resent")
	}
}
func TestContractAgentClaimIDSurvivesLostACK(t *testing.T) {
	dir := t.TempDir()
	requests := make(chan []byte, 4)
	count := 0
	task, release, attempt := UUID(), UUID(), UUID()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != endpoint+"/tasks/"+task+"/claim" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		b, _ := io.ReadAll(r.Body)
		requests <- b
		count++
		var body struct {
			ClaimID string `json:"claim_id"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			t.Error(err)
		}
		var durable Transaction
		if err := readJSON(filepath.Join(dir, "transaction.json"), &durable); err != nil {
			t.Error(err)
		}
		if body.ClaimID == "" || body.ClaimID != durable.ClaimID {
			t.Errorf("claim not persisted before request")
		}
		if count == 1 {
			contractDropACK(t, w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": Claim{TaskID: task, AttemptID: attempt, LeaseToken: "fixture-lease", LeaseExpires: time.Now().Add(time.Hour), Release: Release{ID: release, Version: "v1.1.0"}}})
	}))
	defer srv.Close()
	a := contractAgent(t, dir, srv)
	tx := &Transaction{TaskID: task, ClaimID: UUID(), Candidate: Candidate{TaskID: task, ReleaseID: release, Version: "v1.1.0", Revision: 7}, Phase: "claiming"}
	if err := a.Engine.Save(tx); err != nil {
		t.Fatal(err)
	}
	if err := a.claim(context.Background(), tx); err == nil {
		t.Fatal("lost claim ACK reported success")
	}
	fresh := contractAgent(t, dir, srv)
	recovered, err := fresh.Engine.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = fresh.claim(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	first, second := <-requests, <-requests
	if !bytes.Equal(first, second) {
		t.Fatalf("claim retry changed: %s / %s", first, second)
	}
	durable, err := fresh.Engine.Load()
	if err != nil {
		t.Fatal(err)
	}
	if durable.ClaimID != tx.ClaimID || durable.Claim.AttemptID != attempt || durable.Phase != "claimed" {
		t.Fatalf("claim recovery not durable %+v", durable)
	}
}
func TestContractAgentRunPersistsClaimBeforeRequestAndReusesAfterRestart(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("Agent.Run uses Linux flock; claim/Flush recovery tested portably")
	}
	dir := t.TempDir()
	task, release := UUID(), UUID()
	requests := make(chan string, 4)
	polls := 0
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == endpoint+"/poll" {
			polls++
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"candidate": Candidate{TaskID: task, ReleaseID: release, Version: "v1.1.0", Revision: 7}, "global_enabled": true, "policy": map[string]any{"enabled": true, "revision": 7}}})
			return
		}
		if r.URL.Path != endpoint+"/tasks/"+task+"/claim" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		var body struct {
			ClaimID string `json:"claim_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		var durable Transaction
		if err := readJSON(filepath.Join(dir, "transaction.json"), &durable); err != nil {
			t.Error(err)
		}
		if !uuidRE.MatchString(body.ClaimID) || body.ClaimID != durable.ClaimID {
			t.Error("claim ID missing durable record before HTTP request")
		}
		requests <- body.ClaimID
		contractDropACK(t, w)
	}))
	defer srv.Close()
	a := contractAgent(t, dir, srv)
	for _, p := range a.Engine.Paths {
		contractWrite(t, p, "non executable fixture")
	}
	if err := a.Run(context.Background(), "", "systemd", false); err == nil {
		t.Fatal("lost ACK should fail")
	}
	fresh := contractAgent(t, dir, srv)
	if err := fresh.Run(context.Background(), "", "systemd", false); err != nil {
		t.Fatal(err)
	}
	if len(requests) != 1 {
		t.Fatal("backoff did not suppress immediate retry")
	}
	if err := AtomicJSON(filepath.Join(dir, "retry.json"), retryState{Failures: 1, Next: time.Now().Add(-time.Second)}); err != nil {
		t.Fatal(err)
	}
	if err := fresh.Run(context.Background(), "", "systemd", false); err == nil {
		t.Fatal("lost ACK should fail")
	}
	if first, second := <-requests, <-requests; first != second {
		t.Fatalf("claim changed %s / %s", first, second)
	}
	if polls != 1 {
		t.Fatalf("recovery polled again: %d", polls)
	}
}

func TestPollManualCandidateClaimGates(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("poll attempt uses Linux flock")
	}
	cases := []struct {
		name                             string
		manual, global, policy, disabled bool
		capability                       string
		revision                         int64
		invalidTask, noCandidate         bool
		wantClaim, wantInvalid           bool
	}{
		{name: "automatic_enabled", global: true, policy: true, revision: 7, wantClaim: true},
		{name: "automatic_global_off", policy: true, revision: 7},
		{name: "automatic_policy_off", global: true, revision: 7},
		{name: "automatic_both_off", revision: 7},
		{name: "manual_enabled", manual: true, global: true, policy: true, revision: 7, wantClaim: true},
		{name: "manual_global_off", manual: true, policy: true, revision: 7, wantClaim: true},
		{name: "manual_policy_off", manual: true, global: true, revision: 7, wantClaim: true},
		{name: "manual_both_off", manual: true, revision: 7, wantClaim: true},
		{name: "manual_local_disabled", manual: true, disabled: true, revision: 7},
		{name: "automatic_local_disabled", global: true, policy: true, disabled: true, revision: 7},
		{name: "manual_unsupported", manual: true, capability: "unsupported_init", revision: 7},
		{name: "manual_auto_policy_changed", manual: true, revision: 6, wantClaim: true},
		{name: "automatic_stale_revision", global: true, policy: true, revision: 6, wantInvalid: true},
		{name: "manual_invalid_revision", manual: true, revision: 0, wantInvalid: true},
		{name: "manual_invalid_task", manual: true, revision: 7, invalidTask: true, wantInvalid: true},
		{name: "no_candidate", noCandidate: true, global: true, policy: true, revision: 7},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			task := UUID()
			if tc.invalidTask {
				task = "invalid-task"
			}
			candidate := &Candidate{TaskID: task, ReleaseID: UUID(), Version: "v1.2.3", Revision: tc.revision, Manual: tc.manual}
			if tc.noCandidate {
				candidate = nil
			}
			claims, polls := 0, 0
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case endpoint + "/poll":
					polls++
					var body struct {
						Capability struct {
							Status string  `json:"status"`
							Reason *string `json:"reason"`
						} `json:"capability"`
					}
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					wantReason := tc.capability
					if tc.disabled {
						wantReason = "local_disabled"
					}
					if wantReason != "" {
						if body.Capability.Status != "unsupported" || body.Capability.Reason == nil || *body.Capability.Reason != wantReason {
							t.Errorf("lost local gate: %+v", body.Capability)
						}
					} else if body.Capability.Status != "supported" || body.Capability.Reason != nil {
						t.Errorf("wrong capability: %+v", body.Capability)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"candidate": candidate, "global_enabled": tc.global, "policy": map[string]any{"enabled": tc.policy, "revision": 7}}})
				case endpoint + "/tasks/" + task + "/claim":
					claims++
					var tx Transaction
					if err := readJSON(filepath.Join(dir, "transaction.json"), &tx); err != nil {
						t.Error(err)
					}
					if tx.Candidate.Manual != tc.manual || tx.Manual || tx.TaskID != task {
						t.Errorf("panel manual task must retain normal reporting: %+v", tx)
					}
					// Stop at the claim boundary; this test never downloads or executes binaries.
					w.WriteHeader(http.StatusConflict)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": "task_terminal"}})
				default:
					t.Errorf("unexpected request: %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			a := contractAgent(t, dir, srv)
			a.Config.Disabled = tc.disabled
			for _, path := range a.Engine.Paths {
				contractWrite(t, path, "non executable fixture")
			}
			err := a.runAttempt(context.Background(), tc.capability, "systemd", false)
			if polls != 1 {
				t.Fatalf("wrong poll count: %d", polls)
			}
			if tc.wantClaim {
				var api *APIError
				if claims != 1 || !errors.As(err, &api) || api.Code != "task_terminal" {
					t.Fatalf("manual claim gate failed: claims=%d err=%v", claims, err)
				}
			} else {
				if claims != 0 {
					t.Fatalf("blocked candidate was claimed: %d", claims)
				}
				if tc.wantInvalid {
					if err == nil || err.Error() != "invalid candidate" {
						t.Fatalf("invalid manual candidate accepted: %v", err)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				if _, err := a.Engine.Load(); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("blocked candidate wrote a transaction: %v", err)
				}
			}
		})
	}
}

func TestPollManualDoesNotBypassUnresolvedRecoveryGate(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("poll attempt uses Linux flock")
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("recovery gate must stop before polling or claiming: %s", r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	a := contractAgent(t, t.TempDir(), srv)
	a.Config.Disabled = true
	for _, path := range a.Engine.Paths {
		contractWrite(t, path, "non executable fixture")
	}
	tx := &Transaction{TaskID: UUID(), Candidate: Candidate{Manual: true}, Phase: "rollback_failed", Resolved: true}
	if err := a.Engine.Save(tx); err != nil {
		t.Fatal(err)
	}
	if err := a.runAttempt(context.Background(), "", "systemd", false); err == nil || !strings.Contains(err.Error(), "rollback_failed") {
		t.Fatalf("recovery gate bypassed: %v", err)
	}
}
