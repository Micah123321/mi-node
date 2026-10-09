package updateagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRejectedInstallingGateReusesSequenceAndTerminates(t *testing.T) {
	for _, code := range []string{"policy_disabled", "lease_expired", "batch_paused"} {
		t.Run(code, func(t *testing.T) {
			var received []Event
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var ev Event
				if err := json.NewDecoder(r.Body).Decode(&ev); err != nil {
					t.Error(err)
				}
				received = append(received, ev)
				w.Header().Set("Content-Type", "application/json")
				if ev.State == "installing" {
					w.WriteHeader(409)
					_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code}})
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"accepted_seq": ev.Seq, "state": ev.State, "lease_expires_at": nil}})
			}))
			defer srv.Close()
			a := contractAgent(t, t.TempDir(), srv)
			a.Engine.Emit = a.Queue
			tx := &Transaction{TaskID: "task", LastSeq: 2, AckSeq: 2, Phase: "verifying_artifacts"}
			if err := a.Engine.event(tx, "installing", ""); err != nil {
				t.Fatal(err)
			}
			want := "canceled"
			if code == "lease_expired" {
				want = "failed"
			}
			if len(received) != 2 || received[0].Seq != 3 || received[1].Seq != 3 || received[1].State != want || tx.Phase != want || !tx.Resolved || tx.InstallingACK || len(tx.Pending) != 0 {
				t.Fatalf("invalid gate resolution: %+v %+v", received, tx)
			}
			saved, err := a.Engine.Load()
			if err != nil || saved.Phase != want || !saved.Resolved {
				t.Fatalf("not durable: %+v %v", saved, err)
			}
		})
	}
}

func TestInstallingAckBeforeStartedRecoversThroughRollback(t *testing.T) {
	e, tx, events, _ := contractEngine(t)
	tx.InstallingACK = true
	tx.Phase = "installing"
	tx.Claim.LeaseExpires = time.Now().Add(-time.Minute)
	if err := e.Save(tx); err != nil {
		t.Fatal(err)
	}
	if err := e.Recover(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	contractOldPair(t, e, tx)
	if !tx.Resolved || tx.Phase != "rolled_back" || len(*events) != 2 || (*events)[0] != "rolling_back" {
		t.Fatalf("bad ACK recovery: %+v %v", tx, *events)
	}
}

func TestRecoveryDoesNotRepeatRollingBackAndPreservesRequiredBindings(t *testing.T) {
	e, tx, events, _ := contractEngine(t)
	tx.Started = true
	tx.Phase = "rolling_back"
	e.Snapshot = func() (HealthSnapshot, error) {
		return HealthSnapshot{PID: 20, Start: "new", Readiness: Readiness{Bindings: map[string]bool{"a": true}}}, nil
	}
	e.Health = func(_ context.Context, _ string, b HealthSnapshot, _ bool) (bool, error) {
		if b.PID != 20 || !b.Readiness.Bindings["b"] {
			t.Errorf("lost original bindings: %+v", b)
		}
		return false, nil
	}
	if err := e.Recover(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	if len(*events) != 1 || (*events)[0] != "rolled_back" {
		t.Fatalf("duplicate state: %v", *events)
	}
}
