package updateagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func contractWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
}
func contractContents(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil || string(b) != want {
		t.Fatalf("%s: got %q, error %v; want %q", path, b, err, want)
	}
}
func contractEngine(t *testing.T) (*Engine, *Transaction, *[]string, *int) {
	t.Helper()
	dir := t.TempDir()
	old := "v1.0.0"
	events := []string{}
	restarts := 0
	e := &Engine{Dir: dir, Paths: [2]string{filepath.Join(dir, "mi-node"), filepath.Join(dir, "xbctl")}, Rename: os.Rename, Now: time.Now}
	e.Snapshot = func() (HealthSnapshot, error) {
		return HealthSnapshot{PID: 10, Start: "old", Readiness: Readiness{BootID: "old-boot", Bindings: map[string]bool{"a": true, "b": true}}}, nil
	}
	e.Restart = func(context.Context) error { restarts++; return nil }
	e.Health = func(_ context.Context, v string, before HealthSnapshot, rollback bool) (bool, error) {
		if before.PID != 10 {
			t.Errorf("snapshot lost: %+v", before)
		}
		return false, nil
	}
	e.Emit = func(tx *Transaction, state, code string) error {
		events = append(events, state)
		if state == "installing" {
			tx.InstallingACK = true
		}
		// Emit owns durable event state, matching Agent.Queue.
		tx.Phase = state
		return e.Save(tx)
	}
	tx := &Transaction{TaskID: UUID(), ClaimID: UUID(), OldVersions: Versions{Node: &old, CLI: &old}, TargetVersion: "v1.1.0", Claim: Claim{LeaseExpires: time.Now().Add(time.Hour)}}
	stages := [2]string{filepath.Join(dir, "node-stage"), filepath.Join(dir, "cli-stage")}
	for i, p := range e.Paths {
		contractWrite(t, p, "old-"+filepath.Base(p))
		contractWrite(t, stages[i], "new-"+filepath.Base(p))
	}
	if err := e.Prepare(tx, stages); err != nil {
		t.Fatal(err)
	}
	return e, tx, &events, &restarts
}
func contractOldPair(t *testing.T, e *Engine, tx *Transaction) {
	t.Helper()
	for _, f := range tx.Files {
		contractContents(t, f.Path, "old-"+filepath.Base(f.Path))
		contractContents(t, f.Backup, "old-"+filepath.Base(f.Path))
	}
}

func TestContractInstallingACKFailureDoesNotReplace(t *testing.T) {
	for _, mode := range []string{"transport_failure", "missing_ack"} {
		t.Run(mode, func(t *testing.T) {
			e, tx, _, restarts := contractEngine(t)
			renames := 0
			e.Rename = func(a, b string) error { renames++; return os.Rename(a, b) }
			e.Emit = func(*Transaction, string, string) error {
				if mode == "transport_failure" {
					return errors.New("lost ACK")
				}
				return nil
			}
			if err := e.Install(context.Background(), tx); err == nil {
				t.Fatal("installing without ACK succeeded")
			}
			if renames != 0 || *restarts != 0 || tx.Started {
				t.Fatalf("mutation before ACK: rename=%d restart=%d started=%v", renames, *restarts, tx.Started)
			}
			contractOldPair(t, e, tx)
			for _, f := range tx.Files {
				contractContents(t, f.Stage, "new-"+filepath.Base(f.Path))
			}
		})
	}
}
func TestContractSecondRenameRollsBackPair(t *testing.T) {
	e, tx, events, restarts := contractEngine(t)
	failed := false
	e.Rename = func(a, b string) error {
		if a == tx.Files[1].Stage {
			failed = true
			return errors.New("second rename failed")
		}
		return os.Rename(a, b)
	}
	if err := e.Install(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	contractOldPair(t, e, tx)
	if !failed || *restarts != 1 || !tx.Resolved || tx.Phase != "rolled_back" {
		t.Fatalf("incomplete rollback: %+v restarts=%d", tx, *restarts)
	}
	if !reflect.DeepEqual(*events, []string{"installing", "rolling_back", "rolled_back"}) {
		t.Fatalf("events %v", *events)
	}
}
func TestContractHealthFailureRollsBackPair(t *testing.T) {
	e, tx, events, restarts := contractEngine(t)
	calls := []string{}
	e.Health = func(_ context.Context, v string, _ HealthSnapshot, rollback bool) (bool, error) {
		calls = append(calls, v)
		if !rollback {
			return false, errors.New("health_failed")
		}
		return false, nil
	}
	if err := e.Install(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	contractOldPair(t, e, tx)
	if *restarts != 2 || !reflect.DeepEqual(calls, []string{"v1.1.0", "v1.0.0"}) || tx.Phase != "rolled_back" {
		t.Fatalf("health calls %v restarts %d phase %s", calls, *restarts, tx.Phase)
	}
	if !reflect.DeepEqual(*events, []string{"installing", "verifying", "rolling_back", "rolled_back"}) {
		t.Fatalf("events %v", *events)
	}
}
func TestContractCrashMixedVersionsRecoveredFromJournal(t *testing.T) {
	e, tx, _, restarts := contractEngine(t)
	tx.Started = true
	tx.InstallingACK = true
	tx.Phase = "installing"
	if err := e.Save(tx); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tx.Files[0].Stage, tx.Files[0].Path); err != nil {
		t.Fatal(err)
	}
	recovered, err := e.Load()
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Recover(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	contractOldPair(t, e, recovered)
	persisted, err := e.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !persisted.Resolved || persisted.Phase != "rolled_back" || *restarts != 1 {
		t.Fatalf("recovery not durable: %+v", persisted)
	}
}
func TestContractMissingBackupIsRollbackFailed(t *testing.T) {
	e, tx, events, _ := contractEngine(t)
	tx.Started = true
	if err := os.Rename(tx.Files[0].Stage, tx.Files[0].Path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(tx.Files[1].Backup); err != nil {
		t.Fatal(err)
	}
	if err := e.Save(tx); err != nil {
		t.Fatal(err)
	}
	recovered, err := e.Load()
	if err != nil {
		t.Fatal(err)
	}
	err = e.Recover(context.Background(), recovered)
	if err == nil || !strings.Contains(err.Error(), "rollback_failed") {
		t.Fatalf("got %v", err)
	}
	persisted, err := e.Load()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Phase != "rollback_failed" {
		t.Fatalf("rollback failure not persisted: %+v", persisted)
	}
	contractContents(t, tx.Files[0].Backup, "old-mi-node")
	if (*events)[len(*events)-1] != "rollback_failed" {
		t.Fatalf("events %v", *events)
	}
}
func TestContractSuccessRetainsBothBackups(t *testing.T) {
	e, tx, events, restarts := contractEngine(t)
	if err := e.Install(context.Background(), tx); err != nil {
		t.Fatal(err)
	}
	for _, f := range tx.Files {
		contractContents(t, f.Path, "new-"+filepath.Base(f.Path))
		contractContents(t, f.Backup, "old-"+filepath.Base(f.Path))
	}
	if !tx.Resolved || tx.Phase != "succeeded" || *restarts != 1 {
		t.Fatalf("bad success state %+v", tx)
	}
	if !reflect.DeepEqual(*events, []string{"installing", "verifying", "succeeded"}) {
		t.Fatalf("events %v", *events)
	}
	var metadata struct{ Version string }
	if err := readJSON(filepath.Join(e.Dir, "installed.json"), &metadata); err != nil || metadata.Version != "v1.1.0" {
		t.Fatalf("metadata %+v %v", metadata, err)
	}
}
