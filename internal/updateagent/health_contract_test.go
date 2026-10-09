package updateagent

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestContractHealthStablePIDAndAllBindings(t *testing.T) {
	for _, name := range []string{"stable", "pid_changes_once", "pid_flaps", "missing_binding", "false_binding", "extra_false_binding", "wrong_version", "stale_pid", "stale_boot", "readiness_pid_mismatch", "inactive", "snapshot_error", "readiness_recovers", "legacy_rollback", "legacy_upgrade"} {
		t.Run(name, func(t *testing.T) {
			start := time.Unix(1000, 0)
			clock := start
			before := HealthSnapshot{PID: 10, Start: "old", Readiness: Readiness{BootID: "old-boot", Bindings: map[string]bool{"a": true, "b": true}}}
			snapshot := func() (HealthSnapshot, error) {
				elapsed := clock.Sub(start)
				s := HealthSnapshot{PID: 20, Start: "new", Active: true, Readiness: Readiness{PID: 20, BootID: "new-boot", Version: "v1.1.0", Ready: true, Bindings: map[string]bool{"a": true, "b": true}}}
				switch name {
				case "pid_changes_once":
					if elapsed >= 20*time.Second {
						s.PID = 21
						s.Readiness.PID = 21
						s.Start = "newer"
					}
				case "pid_flaps":
					s.PID = 20 + int(elapsed/(5*time.Second))%2
					s.Readiness.PID = s.PID
				case "missing_binding":
					delete(s.Readiness.Bindings, "b")
				case "false_binding":
					s.Readiness.Bindings["b"] = false
				case "extra_false_binding":
					s.Readiness.Bindings["c"] = false
				case "wrong_version":
					s.Readiness.Version = "v1.0.0"
				case "stale_pid":
					s.PID = 10
					s.Start = "old"
					s.Readiness.PID = 10
				case "stale_boot":
					s.Readiness.BootID = "old-boot"
				case "readiness_pid_mismatch":
					s.Readiness.PID = 99
				case "inactive":
					s.Active = false
				case "snapshot_error":
					return s, errors.New("snapshot failed")
				case "readiness_recovers":
					if elapsed < 20*time.Second {
						s.Readiness.Ready = false
					}
				case "legacy_rollback", "legacy_upgrade":
					s.Readiness = Readiness{}
				}
				return s, nil
			}
			legacy, err := checkHealth(context.Background(), "v1.1.0", before, name == "legacy_rollback", snapshot, func() time.Time { return clock }, func(context.Context) error { clock = clock.Add(5 * time.Second); return nil })
			wantOK := name == "stable" || name == "pid_changes_once" || name == "readiness_recovers" || name == "legacy_rollback"
			if (err == nil) != wantOK {
				t.Fatalf("health error %v want success %v", err, wantOK)
			}
			wantElapsed := 90 * time.Second
			if wantOK {
				wantElapsed = 30 * time.Second
			}
			if name == "pid_changes_once" || name == "readiness_recovers" {
				wantElapsed = 50 * time.Second
			}
			if clock.Sub(start) != wantElapsed {
				t.Fatalf("stability window=%s want %s", clock.Sub(start), wantElapsed)
			}
			if legacy != (name == "legacy_rollback") {
				t.Fatalf("legacy=%v", legacy)
			}
		})
	}
}
func TestContractHealthWaitCancellation(t *testing.T) {
	clock := time.Unix(1000, 0)
	_, err := checkHealth(context.Background(), "v1.1.0", HealthSnapshot{}, false, func() (HealthSnapshot, error) { return HealthSnapshot{}, nil }, func() time.Time { return clock }, func(context.Context) error { return context.Canceled })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}
