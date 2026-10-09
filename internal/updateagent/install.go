package updateagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const ServiceUnit = `[Unit]
Description=mi-node external update agent
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
UMask=0077
ExecStart=/usr/local/bin/xbctl update-agent run
TimeoutStartSec=0
`
const TimerUnit = `[Unit]
Description=mi-node update agent timer

[Timer]
OnBootSec=60s
OnUnitInactiveSec=60s
RandomizedDelaySec=15s
Unit=mi-node-update.service

[Install]
WantedBy=timers.target
`

func Exists(p string) bool { _, e := os.Stat(p); return e == nil }
func Capability() (reason, init string, container bool) {
	init = "other"
	if Exists("/run/systemd/system") {
		init = "systemd"
	} else if Exists("/run/openrc") {
		init = "openrc"
	}
	container = Exists("/.dockerenv") || Exists("/run/.containerenv") || os.Getenv("container") != ""
	if b, e := os.ReadFile("/proc/1/cgroup"); e == nil {
		for _, s := range []string{"docker", "kubepods", "containerd", "lxc"} {
			if strings.Contains(string(b), s) {
				container = true
			}
		}
	}
	if container {
		return "docker", init, true
	}
	if runtime.GOOS != "linux" {
		return "unsupported_os", init, false
	}
	if runtime.GOARCH != "amd64" && runtime.GOARCH != "arm64" {
		return "unsupported_arch", init, false
	}
	if init == "openrc" {
		return "openrc", init, false
	}
	if init != "systemd" {
		return "unsupported_os", init, false
	}
	return "", init, false
}
func CheckLayout(ctx context.Context) error {
	for _, p := range []string{BinaryPath, CLIPath} {
		real, e := filepath.EvalSymlinks(p)
		if e != nil || real != p {
			return fmt.Errorf("shared_binary_layout")
		}
		s, e := os.Stat(p)
		if e != nil || !s.Mode().IsRegular() {
			return fmt.Errorf("shared_binary_layout")
		}
	}
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	if strings.Contains(string(cgroup), "/mi-node.service") {
		return fmt.Errorf("agent must run outside mi-node cgroup")
	}
	units, err := Systemctl(ctx, "list-unit-files", "--type=service", "--no-legend", "--no-pager")
	if err != nil {
		return err
	}
	loaded, err := Systemctl(ctx, "list-units", "--all", "--type=service", "--no-legend", "--no-pager", "--plain")
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	pathRE := regexp.MustCompile("path=([^ ;}]+)")
	found := false
	for _, line := range strings.Split(units+"\n"+loaded, "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 || !strings.HasSuffix(fields[0], ".service") || seen[fields[0]] {
			continue
		}
		unit := fields[0]
		seen[unit] = true
		output, e := Systemctl(ctx, "show", unit, "--property=ExecStart")
		if e != nil {
			return e
		}
		for _, m := range pathRE.FindAllStringSubmatch(output, -1) {
			p, e := filepath.EvalSymlinks(m[1])
			if e == nil && p == BinaryPath {
				if unit != "mi-node.service" {
					return fmt.Errorf("shared_binary_layout")
				}
				found = true
			}
		}
	}
	if !found {
		return fmt.Errorf("fixed mi-node.service executable not found")
	}
	return nil
}
func InstallUnits(ctx context.Context) error {
	if err := atomicBytes("/etc/systemd/system/mi-node-update.service", []byte(ServiceUnit), 0644); err != nil {
		return err
	}
	if err := atomicBytes("/etc/systemd/system/mi-node-update.timer", []byte(TimerUnit), 0644); err != nil {
		return err
	}
	if _, err := Systemctl(ctx, "daemon-reload"); err != nil {
		return err
	}
	_, err := Systemctl(ctx, "enable", "--now", "mi-node-update.timer")
	return err
}
func Disable(ctx context.Context, c Config) error {
	unlock, err := Lock(BinaryPath)
	if err != nil {
		return err
	}
	defer unlock()
	t, err := NewEngine().Load()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if err == nil && !t.Resolved {
		return fmt.Errorf("in-flight transaction must be recovered first")
	}
	c.Disabled = true
	if err = AtomicJSON(filepath.Join(StateDir, "config.json"), c); err != nil {
		return err
	}
	_, err = Systemctl(ctx, "disable", "--now", "mi-node-update.timer")
	return err
}
func Status(c Config) error {
	reason, init, container := Capability()
	status := map[string]any{"installation_id": c.InstallationID, "authority_panel_url": c.Authority, "authority_scope": c.Scope, "enrolled": c.Enrolled, "disabled": c.Disabled, "capability_reason": reason, "init": init, "container": container}
	t, err := NewEngine().Load()
	if err == nil {
		status["transaction"] = map[string]any{"task_id": t.TaskID, "phase": t.Phase, "resolved": t.Resolved, "last_seq": t.LastSeq, "acked_seq": t.AckSeq, "pending_events": len(t.Pending)}
	} else if !os.IsNotExist(err) {
		return err
	}
	return json.NewEncoder(os.Stdout).Encode(status)
}
