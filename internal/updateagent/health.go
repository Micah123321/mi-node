package updateagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const SystemctlTimeout = 180 * time.Second

type Readiness struct {
	PID      int             `json:"pid"`
	BootID   string          `json:"boot_id"`
	Version  string          `json:"version"`
	Ready    bool            `json:"ready"`
	Bindings map[string]bool `json:"bindings_ready"`
}
type HealthSnapshot struct {
	PID       int
	Start     string
	Active    bool
	Readiness Readiness
}

func Systemctl(ctx context.Context, args ...string) (string, error) {
	bounded, cancel := context.WithTimeout(ctx, SystemctlTimeout)
	defer cancel()
	b, err := exec.CommandContext(bounded, "systemctl", args...).Output()
	if err != nil {
		return "", fmt.Errorf("systemctl failed")
	}
	return strings.TrimSpace(string(b)), nil
}
func Restart(ctx context.Context) error {
	if err := os.Remove(ReadinessPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	_, err := Systemctl(ctx, "restart", "mi-node.service")
	return err
}
func Snapshot() (HealthSnapshot, error) {
	var s HealthSnapshot
	out, err := Systemctl(context.Background(), "show", "mi-node.service", "--property=MainPID,ActiveState")
	if err != nil {
		return s, err
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		if k == "MainPID" {
			s.PID, _ = strconv.Atoi(v)
		}
		if k == "ActiveState" {
			s.Active = v == "active"
		}
	}
	if s.PID > 0 {
		stat, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", s.PID))
		if err != nil {
			return s, err
		}
		end := strings.LastIndex(string(stat), ")")
		if end < 0 {
			return s, fmt.Errorf("invalid process stat")
		}
		fields := strings.Fields(string(stat)[end+1:])
		if len(fields) < 20 {
			return s, fmt.Errorf("invalid process stat")
		}
		s.Start = fields[19]
	}
	if err = secureFile(ReadinessPath); err == nil {
		b, e := os.ReadFile(ReadinessPath)
		if e == nil {
			_ = json.Unmarshal(b, &s.Readiness)
		}
	}
	return s, nil
}
func Healthy(ctx context.Context, version string, before HealthSnapshot, rollback bool) (bool, error) {
	return checkHealth(ctx, version, before, rollback, Snapshot, time.Now, func(ctx context.Context) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
			return nil
		}
	})
}
func checkHealth(ctx context.Context, version string, before HealthSnapshot, rollback bool, snapshot func() (HealthSnapshot, error), now func() time.Time, wait func(context.Context) error) (bool, error) {
	deadline := now().Add(90 * time.Second)
	var stable time.Time
	identity := ""
	legacy := false
	for now().Before(deadline) {
		s, err := snapshot()
		valid := err == nil && s.Active && s.PID > 0 && s.Start != "" && (s.PID != before.PID || s.Start != before.Start)
		r := s.Readiness
		legacy = rollback && r.BootID == ""
		if valid && !legacy {
			valid = r.PID == s.PID && r.BootID != "" && r.BootID != before.Readiness.BootID && r.Version == version && r.Ready && len(r.Bindings) > 0
			for key := range before.Readiness.Bindings {
				if !r.Bindings[key] {
					valid = false
				}
			}
			for _, ready := range r.Bindings {
				if !ready {
					valid = false
				}
			}
		}
		current := fmt.Sprintf("%d/%s/%s/%t", s.PID, s.Start, r.BootID, legacy)
		if !valid {
			stable = time.Time{}
			identity = ""
		} else if current != identity {
			stable = now()
			identity = current
		} else if now().Sub(stable) >= 30*time.Second {
			return legacy, nil
		}
		if err := wait(ctx); err != nil {
			return false, err
		}
	}
	return false, fmt.Errorf("health_failed")
}
