package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"

	"github.com/micah123321/mi-node/internal/updateagent"
)

func manualServiceEngine(e *updateagent.Engine, restart func() error, state func() string) {
	// Manual upgrades retain legacy systemd/OpenRC service-state health semantics.
	e.Snapshot = func() (updateagent.HealthSnapshot, error) { return updateagent.HealthSnapshot{}, nil }
	e.Restart = func(context.Context) error { return restart() }
	e.Health = func(context.Context, string, updateagent.HealthSnapshot, bool) (bool, error) {
		s := state()
		if s != "active" && s != "running" {
			return true, fmt.Errorf("service is %s", s)
		}
		return true, nil
	}
}

func manualUpgrade(args []string, e *updateagent.Engine, lock func(string) (func(), error), download func(string, string) error, validate func(string, ...string) error) error {
	if len(args) == 1 && args[0] == "--recover" {
		unlock, err := lock(e.Paths[0])
		if err != nil {
			return err
		}
		defer unlock()
		tx, err := e.Load()
		if err != nil {
			return err
		}
		if !tx.Manual {
			return fmt.Errorf("automatic transaction must be recovered by update-agent")
		}
		if tx.Phase == "rollback_failed" {
			return fmt.Errorf("rollback failed; inspect retained backups before recovery")
		}
		if tx.Resolved {
			return nil
		}
		if !tx.Started {
			for _, f := range tx.Files {
				h, err := updateagent.Hash(f.Path)
				if err != nil || h != f.OldSHA {
					return fmt.Errorf("original binary changed; inspect transaction before recovery")
				}
			}
			tx.Resolved, tx.Phase = true, "failed"
			return e.Save(tx)
		}
		return e.Recover(context.Background(), tx)
	}
	version := "latest"
	for i := 0; i < len(args); i++ {
		if args[i] != "--version" || i+1 >= len(args) {
			return fmt.Errorf("usage: xbctl upgrade [--version <tag>]")
		}
		i++
		version = args[i]
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return fmt.Errorf("unsupported architecture: %s", runtime.GOARCH)
	}
	// Resolve targets before locking so symlink aliases update the same files.
	for i, p := range e.Paths {
		canonical, err := filepath.EvalSymlinks(p)
		if err != nil {
			return err
		}
		e.Paths[i], err = filepath.Abs(canonical)
		if err != nil {
			return err
		}
	}
	unlock, err := lock(e.Paths[0])
	if err != nil {
		return err
	}
	defer unlock()
	if err = e.ManualAllowed(); err != nil {
		return err
	}
	var staged [2]string
	for i, name := range []string{"mi-node", "xbctl"} {
		f, err := os.CreateTemp(filepath.Dir(e.Paths[i]), "."+name+"-manual-*")
		if err != nil {
			return err
		}
		staged[i] = f.Name()
		f.Close()
		defer os.Remove(staged[i])
		url := resolveDownloadURL(fmt.Sprintf("%s-linux-%s", name, runtime.GOARCH), version)
		if err = download(url, staged[i]); err != nil {
			return err
		}
		if err = os.Chmod(staged[i], 0755); err != nil {
			return err
		}
		arg := "version"
		if i == 0 {
			arg = "-v"
		}
		if err = validate(staged[i], arg); err != nil {
			return err
		}
		f, err = os.OpenFile(staged[i], os.O_RDWR, 0)
		if err != nil {
			return err
		}
		syncErr := f.Sync()
		closeErr := f.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	// Historical releases have no required published SHA: executable checks and
	// journal hashes do not authenticate the downloaded release.
	return e.InstallManual(context.Background(), staged, version)
}

func executeManualUpgrade(args []string) error {
	e := updateagent.NewEngine()
	manualServiceEngine(e, func() error {
		if detectInitSystem() == initSystemSystemd {
			if err := runCommand("systemctl", "daemon-reload"); err != nil {
				return err
			}
		}
		return runServiceCommand("restart")
	}, serviceState)
	return manualUpgrade(args, e, updateagent.Lock, downloadFile, func(path string, args ...string) error {
		if out, err := exec.Command(path, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("binary version check failed: %w: %s", err, out)
		}
		return nil
	})
}
