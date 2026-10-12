package updateagent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const bootstrapService = "mi-node-update-bootstrap.service"
const bootstrapTimer = "mi-node-update-bootstrap.timer"
const bootstrapTimerUnit = `[Unit]
Description=mi-node automatic enrollment retry

[Timer]
OnBootSec=10s
OnUnitInactiveSec=60s
RandomizedDelaySec=15s
Unit=mi-node-update-bootstrap.service

[Install]
WantedBy=timers.target
`

// Bootstrap only asks systemd to start a separate service. Network enrollment,
// layout inspection and transaction recovery belong to that service cgroup.
func Bootstrap(ctx context.Context, configPath string) error {
	reason, _, _ := Capability()
	if os.Geteuid() != 0 || reason != "" {
		return nil
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if executable != BinaryPath {
		return nil
	}
	if err := checkBinaryLayout(); err != nil {
		return err
	}
	return bootstrapUnits(ctx, "/etc/systemd/system", configPath, StateDir, Systemctl)
}

func unitPath(path string) (string, error) {
	if strings.ContainsAny(path, "\x00\r\n") {
		return "", fmt.Errorf("invalid systemd path")
	}
	escaped := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%").Replace(path)
	return "\"" + escaped + "\"", nil
}

func bootstrapServiceUnit(configPath, environmentPath string) (string, error) {
	path, err := filepath.Abs(configPath)
	if err != nil {
		return "", err
	}
	quoted, err := unitPath(path)
	if err != nil {
		return "", err
	}
	environment, err := filepath.Abs(environmentPath)
	if err != nil || strings.ContainsAny(environment, "\x00\r\n") {
		return "", fmt.Errorf("invalid systemd environment path")
	}
	// EnvironmentFile consumes the whole absolute path; literal quotes become part of it.
	environment = strings.ReplaceAll(environment, "%", "%%")
	return fmt.Sprintf(`[Unit]
Description=mi-node automatic update enrollment
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
UMask=0077
EnvironmentFile=%s
ExecStart=/usr/local/bin/xbctl update-agent auto-install -config %s
TimeoutStartSec=0
`, environment, strings.ReplaceAll(quoted, "$", "$$")), nil
}

type systemctlFunc func(context.Context, ...string) (string, error)

func ensureUnits(ctx context.Context, directory string, units map[string]string, ctl systemctlFunc) error {
	changed := false
	for name, text := range units {
		path := filepath.Join(directory, name)
		current, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err == nil && bytes.Equal(current, []byte(text)) {
			continue
		}
		if err := atomicBytes(path, []byte(text), 0644); err != nil {
			return err
		}
		changed = true
	}
	if changed {
		_, err := ctl(ctx, "daemon-reload")
		return err
	}
	return nil
}

func bootstrapUnits(ctx context.Context, directory, configPath, stateDirectory string, ctl systemctlFunc) error {
	environmentPath := filepath.Join(stateDirectory, "bootstrap", "environment.env")
	service, err := bootstrapServiceUnit(configPath, environmentPath)
	if err != nil {
		return err
	}
	if err := writeBootstrapEnvironment(configPath, stateDirectory); err != nil {
		return err
	}
	if err := ensureUnits(ctx, directory, map[string]string{bootstrapService: service, bootstrapTimer: bootstrapTimerUnit}, ctl); err != nil {
		return err
	}
	if _, err := ctl(ctx, "enable", "--now", bootstrapTimer); err != nil {
		return err
	}
	_, err = ctl(ctx, "start", "--no-block", bootstrapService)
	return err
}

func checkBinaryLayout() error {
	for _, p := range []string{BinaryPath, CLIPath} {
		real, err := filepath.EvalSymlinks(p)
		if err != nil || real != p {
			return fmt.Errorf("shared_binary_layout")
		}
		info, err := os.Stat(p)
		if err != nil || !info.Mode().IsRegular() {
			return fmt.Errorf("shared_binary_layout")
		}
	}
	return nil
}
