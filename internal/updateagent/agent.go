package updateagent

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

type Config struct {
	InstallationID  string          `json:"installation_id"`
	Authority       string          `json:"authority_panel_url"`
	Scope           json.RawMessage `json:"authority_scope"`
	Token           string          `json:"agent_token"`
	EnrollmentID    string          `json:"enrollment_id"`
	Enrolled        bool            `json:"enrolled"`
	Disabled        bool            `json:"disabled"`
	MachineIdentity string          `json:"machine_identity"`
}
type Enrollment struct {
	ID         string `json:"enrollment_id"`
	Secret     string `json:"enrollment_secret"`
	InstanceID string `json:"instance_id"`
}

func ReadEnrollment(path string) (Enrollment, error) {
	var t Enrollment
	if e := secureFile(path); e != nil {
		return t, e
	}
	e := readJSON(path, &t)
	if e == nil && (!uuidRE.MatchString(t.ID) || t.Secret == "" || t.InstanceID == "") {
		e = fmt.Errorf("invalid enrollment file")
	}
	return t, e
}
func machineIdentity() (string, error) { return Hash("/etc/machine-id") }
func LoadConfig() (Config, error) {
	var c Config
	if err := secureFile(filepath.Join(StateDir, "config.json")); err != nil {
		return c, err
	}
	err := readJSON(filepath.Join(StateDir, "config.json"), &c)
	if err != nil {
		return c, err
	}
	identity, err := machineIdentity()
	if err != nil || c.MachineIdentity != identity {
		return c, fmt.Errorf("cloned installation: re-enrollment required")
	}
	return c, nil
}
func Enroll(ctx context.Context, authority string, ticket Enrollment, auth map[string]any) (Config, error) {
	var c Config
	client, err := NewClient(authority, "")
	if err != nil {
		return c, err
	}
	if err = os.MkdirAll(StateDir, 0700); err != nil {
		return c, err
	}
	if err = os.Chmod(StateDir, 0700); err != nil {
		return c, err
	}
	path := filepath.Join(StateDir, "config.json")
	c, err = LoadConfig()
	if errors.Is(err, os.ErrNotExist) {
		var token [32]byte
		if _, err = rand.Read(token[:]); err != nil {
			return c, err
		}
		identity, e := machineIdentity()
		if e != nil {
			return c, e
		}
		c = Config{InstallationID: UUID(), Authority: strings.TrimRight(authority, "/"), Token: base64.RawURLEncoding.EncodeToString(token[:]), EnrollmentID: ticket.ID, MachineIdentity: identity}
		if err = AtomicJSON(path, c); err != nil {
			return c, err
		}
	} else if err != nil {
		return c, err
	}
	if c.Authority != strings.TrimRight(authority, "/") || c.EnrollmentID != ticket.ID {
		return c, fmt.Errorf("existing installation cannot change authority/enrollment")
	}
	if c.Enrolled {
		return c, nil
	}
	var result struct {
		InstallationID string          `json:"installation_id"`
		Scope          json.RawMessage `json:"scope"`
	}
	body := map[string]any{"protocol_version": 1, "enrollment_id": ticket.ID, "enrollment_secret": ticket.Secret, "installation_id": c.InstallationID, "agent_token": c.Token, "auth": auth}
	if err = client.Post(ctx, "/enroll", body, &result); err != nil {
		return c, err
	}
	if result.InstallationID != c.InstallationID || len(result.Scope) == 0 {
		return c, fmt.Errorf("invalid enrollment response")
	}
	c.Scope = result.Scope
	c.Enrolled = true
	err = AtomicJSON(path, c)
	return c, err
}

type Agent struct {
	Config Config
	Client *Client
	Engine *Engine
}

func NewAgent(c Config) (*Agent, error) {
	client, err := NewClient(c.Authority, c.Token)
	if err != nil {
		return nil, err
	}
	a := &Agent{c, client, NewEngine()}
	a.Engine.Emit = a.Queue
	return a, nil
}
func (a *Agent) Queue(t *Transaction, state, code string) error {
	if t.Manual {
		t.Phase = state
		return a.Engine.Save(t)
	}
	var c *string
	if code != "" {
		c = &code
	}
	t.LastSeq++
	t.Phase = state
	event := Event{InstallationID: a.Config.InstallationID, AttemptID: t.Claim.AttemptID, LeaseToken: t.Claim.LeaseToken, Seq: t.LastSeq, State: state, OccurredAt: time.Now().UTC().Format(time.RFC3339), Code: c, Observed: observeVersions(a.Engine.Paths)}
	t.Pending = append(t.Pending, event)
	if err := a.Engine.Save(t); err != nil {
		return err
	}
	return a.Flush(context.Background(), t)
}
func (a *Agent) Flush(ctx context.Context, t *Transaction) error {
	for len(t.Pending) > 0 {
		ev := t.Pending[0]
		var ack struct {
			Seq          int64      `json:"accepted_seq"`
			State        string     `json:"state"`
			LeaseExpires *time.Time `json:"lease_expires_at"`
		}
		err := a.Client.Post(ctx, "/tasks/"+t.TaskID+"/events", ev, &ack)
		if err != nil {
			var api *APIError
			if ev.State == "installing" && !t.Started && !t.InstallingACK && errors.As(err, &api) {
				switch api.Code {
				case "lease_expired", "policy_disabled", "batch_paused", "batch_canceled", "release_revoked", "policy_changed":
					// A gate rejection proves this sequence was not accepted. Reuse it.
					state, code := "canceled", api.Code
					if code == "lease_expired" {
						state = "failed"
					}
					if code == "policy_changed" {
						code = "policy_disabled"
					}
					ev.State, ev.Code = state, &code
					t.Pending[0] = ev
					t.Phase, t.Resolved = state, true
					if saveErr := a.Engine.Save(t); saveErr != nil {
						return saveErr
					}
					continue
				}
			}
			return err
		}
		if ack.Seq != ev.Seq {
			return fmt.Errorf("invalid event ACK")
		}
		t.AckSeq = ack.Seq
		t.Pending = t.Pending[1:]
		if ev.State == "installing" {
			if ack.LeaseExpires == nil {
				return fmt.Errorf("missing installing lease")
			}
			t.InstallingACK = true
			t.Claim.LeaseExpires = *ack.LeaseExpires
		}
		if err = a.Engine.Save(t); err != nil {
			return err
		}
	}
	return nil
}
func observeVersions(paths [2]string) Versions {
	var result Versions
	for i, p := range paths {
		v, e := Probe(p)
		if e != nil {
			continue
		}
		s := v.Version
		if i == 0 {
			result.Node = &s
		} else {
			result.CLI = &s
		}
	}
	return result
}
func observeHashes(paths [2]string) Versions {
	var result Versions
	for i, p := range paths {
		h, e := Hash(p)
		if e != nil {
			continue
		}
		if i == 0 {
			result.Node = &h
		} else {
			result.CLI = &h
		}
	}
	return result
}
func (a *Agent) claim(ctx context.Context, t *Transaction) error {
	var c Claim
	if err := a.Client.Post(ctx, "/tasks/"+t.TaskID+"/claim", map[string]any{"installation_id": a.Config.InstallationID, "claim_id": t.ClaimID, "policy_revision": t.Candidate.Revision}, &c); err != nil {
		var api *APIError
		if errors.As(err, &api) {
			switch api.Code {
			case "policy_changed", "task_terminal", "not_found", "release_revoked", "downgrade_forbidden", "version_conflict", "binary_version_mismatch":
				t.Phase, t.Resolved = "claim_rejected", true
				if saveErr := a.Engine.Save(t); saveErr != nil {
					return saveErr
				}
			}
		}
		return err
	}
	if c.TaskID != t.TaskID || !uuidRE.MatchString(c.AttemptID) || c.LeaseToken == "" || c.Release.ID != t.Candidate.ReleaseID || c.Release.Version != t.Candidate.Version {
		return fmt.Errorf("invalid claim response")
	}
	t.Claim = c
	t.Phase = "claimed"
	t.TargetVersion = c.Release.Version
	return a.Engine.Save(t)
}
func (a *Agent) heartbeat(ctx context.Context, t *Transaction) (bool, error) {
	var response struct {
		Allow   bool      `json:"allow_start"`
		Expires time.Time `json:"lease_expires_at"`
	}
	err := a.Client.Post(ctx, "/tasks/"+t.TaskID+"/heartbeat", map[string]any{"installation_id": a.Config.InstallationID, "attempt_id": t.Claim.AttemptID, "lease_token": t.Claim.LeaseToken}, &response)
	return response.Allow && time.Now().Before(response.Expires), err
}
func (a *Agent) Execute(ctx context.Context, t *Transaction) error {
	pair, err := t.Claim.Release.Pair(runtime.GOARCH)
	if err != nil {
		return a.finish(t, "failed", "arch_mismatch")
	}
	old := observeVersions(a.Engine.Paths)
	t.OldVersions = old
	if old.Node == nil || old.CLI == nil || *old.Node != *old.CLI {
		return a.finish(t, "failed", "binary_version_mismatch")
	}
	cmp, err := CompareVersions(t.TargetVersion, *old.Node)
	if err != nil || cmp < 0 {
		return a.finish(t, "failed", "binary_version_mismatch")
	}
	hashes := observeHashes(a.Engine.Paths)
	if cmp == 0 {
		if hashes.Node != nil && hashes.CLI != nil && *hashes.Node == pair[0].SHA && *hashes.CLI == pair[1].SHA {
			return a.finish(t, "skipped", "already_current")
		}
		return a.finish(t, "failed", "binary_version_mismatch")
	}
	work, cancel := context.WithCancel(ctx)
	defer cancel()
	var allowed atomic.Bool
	allowed.Store(true)
	// Heartbeats do not mutate the journal. The installing ACK supplies its lease.
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-work.Done():
				return
			case <-ticker.C:
				ok, e := a.heartbeat(work, t)
				if e != nil || !ok {
					allowed.Store(false)
				}
			}
		}
	}()
	defer func() { cancel(); <-hbDone }()
	if err = a.Queue(t, "downloading", ""); err != nil {
		return err
	}
	var staged [2]string
	for i, artifact := range pair {
		staged[i] = filepath.Join(filepath.Dir(a.Engine.Paths[i]), "."+artifact.Component+".stage-"+t.ClaimID)
		_ = os.Remove(staged[i])
		if !allowed.Load() {
			return a.finish(t, "canceled", "policy_disabled")
		}
		if err = Download(work, DownloadClient(), artifact, staged[i]); err != nil {
			code := err.Error()
			if code != "checksum_mismatch" && code != "size_mismatch" {
				code = "download_failed"
			}
			return a.finish(t, "failed", code)
		}
	}
	if err = a.Queue(t, "verifying_artifacts", ""); err != nil {
		return err
	}
	for i, p := range staged {
		if err = ValidateELF(p, pair[i].Arch); err != nil {
			return a.finish(t, "failed", "arch_mismatch")
		}
	}
	// Neither executable is invoked before BOTH hashes/sizes/ELF headers pass.
	for i, p := range staged {
		v, e := Probe(p)
		if e != nil || v.Version != t.TargetVersion || v.OS != "linux" || v.Arch != pair[i].Arch {
			return a.finish(t, "failed", "binary_version_mismatch")
		}
	}
	if ok, e := a.heartbeat(ctx, t); e != nil || !ok || !allowed.Load() {
		return a.finish(t, "canceled", "policy_disabled")
	}
	if err = a.Engine.Prepare(t, staged); err != nil {
		return a.finish(t, "failed", "backup_failed")
	}
	return a.Engine.Install(ctx, t)
}
func (a *Agent) finish(t *Transaction, state, code string) error {
	t.Resolved = true
	return a.Queue(t, state, code)
}
func (a *Agent) runAttempt(ctx context.Context, capability string, init string, container bool) error {
	unlock, err := Lock(a.Engine.Paths[0])
	if err != nil {
		return err
	}
	defer unlock()
	// Manual upgrades archive unsent automatic facts before taking their journal.
	archives, err := filepath.Glob(filepath.Join(a.Engine.Dir, "pending-*.json"))
	if err != nil {
		return err
	}
	for _, p := range archives {
		var t Transaction
		if err = readJSON(p, &t); err != nil {
			return err
		}
		previousPath := a.Engine.JournalPath
		a.Engine.JournalPath = p
		err = a.Flush(ctx, &t)
		a.Engine.JournalPath = previousPath
		if err != nil {
			return err
		}
		if err = os.Remove(p); err != nil {
			return err
		}
	}
	t, err := a.Engine.Load()
	if err == nil {
		if t.Started && !t.Resolved {
			if err = a.Engine.Recover(ctx, t); err != nil {
				return err
			}
		}
		if !t.Manual {
			if err = a.Flush(ctx, t); err != nil {
				return err
			}
		}
		if t.Phase == "rollback_failed" {
			return fmt.Errorf("rollback_failed; local repair required before further updates")
		}
		if !t.Resolved {
			if t.InstallingACK {
				return a.Engine.Recover(ctx, t)
			}
			if t.Claim.AttemptID == "" {
				if err = a.claim(ctx, t); err != nil {
					return err
				}
				return a.Execute(ctx, t)
			}
			// After an interrupted pre-install phase, retain originals and end the attempt.
			code := "download_failed"
			if t.InstallingACK {
				code = "lease_expired"
			}
			return a.finish(t, "failed", code)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	var reason *string
	if capability != "" {
		reason = &capability
	}
	status := "supported"
	if reason != nil {
		status = "unsupported"
	}
	if a.Config.Disabled {
		r := "local_disabled"
		reason = &r
		status = "unsupported"
	}
	body := map[string]any{"protocol_version": 1, "installation_id": a.Config.InstallationID, "versions": observeVersions(a.Engine.Paths), "installed_sha256": observeHashes(a.Engine.Paths), "runtime": map[string]any{"os": runtime.GOOS, "arch": runtime.GOARCH, "init": init, "container": container}, "capability": map[string]any{"status": status, "reason": reason}, "active_attempt": nil}
	var poll struct {
		Candidate *Candidate `json:"candidate"`
		Global    bool       `json:"global_enabled"`
		Policy    struct {
			Enabled  bool  `json:"enabled"`
			Revision int64 `json:"revision"`
		} `json:"policy"`
		After int `json:"poll_after_seconds"`
	}
	if err = a.Client.Post(ctx, "/poll", body, &poll); err != nil {
		return err
	}
	if poll.Candidate == nil || !poll.Global || !poll.Policy.Enabled || reason != nil {
		return nil
	}
	if !uuidRE.MatchString(poll.Candidate.TaskID) || poll.Candidate.Revision != poll.Policy.Revision {
		return fmt.Errorf("invalid candidate")
	}
	t = &Transaction{TaskID: poll.Candidate.TaskID, ClaimID: UUID(), Candidate: *poll.Candidate, CredentialRef: "config.json", Phase: "claiming"}
	if err = a.Engine.Save(t); err != nil {
		return err
	}
	if err = a.claim(ctx, t); err != nil {
		return err
	}
	return a.Execute(ctx, t)
}
