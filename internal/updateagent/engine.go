package updateagent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type FileState struct {
	Path   string
	Stage  string
	Backup string
	OldSHA string
	NewSHA string
}
type Transaction struct {
	TaskID        string
	ClaimID       string
	Candidate     Candidate
	Claim         Claim
	CredentialRef string
	Files         [2]FileState
	OldVersions   Versions
	TargetVersion string
	Before        HealthSnapshot
	Phase         string
	LastSeq       int64
	AckSeq        int64
	Pending       []Event
	InstallingACK bool
	Started       bool
	Resolved      bool
	Manual        bool
}
type Engine struct {
	JournalPath string
	Dir         string
	Paths       [2]string
	Rename      func(string, string) error
	Restart     func(context.Context) error
	Health      func(context.Context, string, HealthSnapshot, bool) (bool, error)
	Snapshot    func() (HealthSnapshot, error)
	Emit        func(*Transaction, string, string) error
	Now         func() time.Time
}

func NewEngine() *Engine {
	return &Engine{Dir: StateDir, Paths: [2]string{BinaryPath, CLIPath}, Rename: os.Rename, Restart: Restart, Health: Healthy, Snapshot: Snapshot, Now: time.Now}
}
func (e *Engine) journalPath() string {
	if e.JournalPath != "" {
		return e.JournalPath
	}
	return filepath.Join(e.Dir, "transaction.json")
}
func (e *Engine) Save(t *Transaction) error {
	return AtomicJSON(e.journalPath(), t)
}
func (e *Engine) Load() (*Transaction, error) {
	var t Transaction
	err := readJSON(e.journalPath(), &t)
	return &t, err
}
func (e *Engine) event(t *Transaction, state, code string) error {
	previous := t.Phase
	if e.Emit != nil {
		if err := e.Emit(t, state, code); err != nil {
			return err
		}
	}
	// The protocol emitter may replace a rejected gate with a terminal event.
	if t.Phase == previous {
		t.Phase = state
	}
	return e.Save(t)
}
func (e *Engine) Prepare(t *Transaction, staged [2]string) error {
	t.Before = HealthSnapshot{}
	if e.Snapshot != nil {
		s, err := e.Snapshot()
		if err != nil {
			return err
		}
		t.Before = s
	}
	for i, p := range e.Paths {
		old, err := Hash(p)
		if err != nil {
			return err
		}
		newSHA, err := Hash(staged[i])
		if err != nil {
			return err
		}
		t.Files[i] = FileState{p, staged[i], p + ".backup-" + t.ClaimID, old, newSHA}
	}
	if err := e.Save(t); err != nil {
		return err
	}
	for _, f := range t.Files {
		if err := copyDurable(f.Path, f.Backup); err != nil {
			return fmt.Errorf("backup_failed")
		}
		h, err := Hash(f.Backup)
		if err != nil || h != f.OldSHA {
			return fmt.Errorf("backup_failed")
		}
	}
	return e.Save(t)
}
func (e *Engine) Install(ctx context.Context, t *Transaction) error {
	// The server ACK and its durable local record precede any binary mutation.
	if !t.Manual {
		if err := e.event(t, "installing", ""); err != nil {
			return err
		}
		if t.Resolved {
			return nil
		}
		if !t.InstallingACK {
			return fmt.Errorf("installing ACK missing")
		}
		if !e.Now().Before(t.Claim.LeaseExpires) {
			return e.Rollback(ctx, t, "lease_expired")
		}
	}
	t.Started = true
	t.Phase = "installing"
	if err := e.Save(t); err != nil {
		return err
	}
	for _, f := range t.Files {
		if err := e.Rename(f.Stage, f.Path); err != nil {
			return e.Rollback(ctx, t, "restart_failed")
		}
		if err := syncDir(filepath.Dir(f.Path)); err != nil {
			return e.Rollback(ctx, t, "restart_failed")
		}
		if err := e.Save(t); err != nil {
			return err
		}
	}
	if err := e.Restart(ctx); err != nil {
		return e.Rollback(ctx, t, "restart_failed")
	}
	// Reporting failure after the gate never interrupts local recovery/health.
	_ = e.event(t, "verifying", "")
	if _, err := e.Health(ctx, t.TargetVersion, t.Before, false); err != nil {
		return e.Rollback(ctx, t, "health_failed")
	}
	t.Resolved = true
	if err := AtomicJSON(filepath.Join(e.Dir, "installed.json"), map[string]any{"version": t.TargetVersion, "files": t.Files, "completed_at": e.Now().UTC()}); err != nil {
		return err
	}
	return e.event(t, "succeeded", "")
}
func (e *Engine) Rollback(ctx context.Context, t *Transaction, code string) error {
	if t.Phase != "rolling_back" {
		_ = e.event(t, "rolling_back", code)
	}
	for _, f := range t.Files {
		h, err := Hash(f.Backup)
		if err != nil || h != f.OldSHA {
			return e.rollbackFailed(t)
		}
		h, err = Hash(f.Path)
		if err == nil && h == f.OldSHA {
			continue
		}
		temp := f.Path + ".restore-" + t.ClaimID
		_ = os.Remove(temp)
		if err = copyDurable(f.Backup, temp); err != nil {
			return e.rollbackFailed(t)
		}
		if err = e.Rename(temp, f.Path); err != nil {
			return e.rollbackFailed(t)
		}
		if err = syncDir(filepath.Dir(f.Path)); err != nil {
			return e.rollbackFailed(t)
		}
	}
	before, _ := e.Snapshot()
	// Restart identity is current; required bindings remain the pre-upgrade set.
	before.Readiness.Bindings = t.Before.Readiness.Bindings
	if err := e.Restart(ctx); err != nil {
		return e.rollbackFailed(t)
	}
	old := ""
	if t.OldVersions.Node != nil {
		old = *t.OldVersions.Node
	}
	legacy, err := e.Health(ctx, old, before, true)
	if err != nil {
		return e.rollbackFailed(t)
	}
	if legacy {
		code = "rollback_legacy_health"
	}
	t.Resolved = true
	return e.event(t, "rolled_back", code)
}
func (e *Engine) rollbackFailed(t *Transaction) error {
	t.Resolved = true
	_ = e.event(t, "rollback_failed", "rollback_failed")
	return fmt.Errorf("rollback_failed; journal and backups retained")
}
func (e *Engine) Recover(ctx context.Context, t *Transaction) error {
	if t.Resolved {
		return nil
	}
	if !t.Started && !t.InstallingACK {
		return nil
	}
	// Even two new hashes do not prove a completed health window after a crash.
	return e.Rollback(ctx, t, "health_failed")
}
func (e *Engine) ManualAllowed() error {
	t, err := e.Load()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !t.Resolved || t.Phase == "rollback_failed" {
		return fmt.Errorf("unresolved update transaction; run update-agent run first")
	}
	// Keep unsent automatic results independent of the next manual journal.
	if len(t.Pending) > 0 {
		if err = AtomicJSON(filepath.Join(e.Dir, "pending-"+t.ClaimID+".json"), t); err != nil {
			return err
		}
	}
	return nil
}
