package updateagent

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
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
