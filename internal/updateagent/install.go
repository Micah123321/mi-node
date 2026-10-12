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
	return platformCapability(runtime.GOOS, runtime.GOARCH, init, container)
}

func platformCapability(goos, arch, init string, container bool) (string, string, bool) {
	if container {
		return "docker", init, true
	}
	if goos != "linux" {
		return "unsupported_os", init, false
	}
	if arch != "amd64" && arch != "arm64" {
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
	if err := checkBinaryLayout(); err != nil {
		return err
	}
	cgroup, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return err
	}
	if strings.Contains(string(cgroup), "/mi-node.service") {
		return fmt.Errorf("agent must run outside mi-node cgroup")
	}
	return checkServiceLayout(ctx, Systemctl, filepath.EvalSymlinks)
}

func checkServiceLayout(ctx context.Context, ctl systemctlFunc, resolve func(string) (string, error)) error {
	units, err := ctl(ctx, "list-unit-files", "--type=service", "--no-legend", "--no-pager")
	if err != nil {
		return err
	}
	loaded, err := ctl(ctx, "list-units", "--all", "--type=service", "--no-legend", "--no-pager", "--plain")
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
		// A template is not a runnable unit. Its actual instances are listed separately.
		if strings.HasSuffix(unit, "@.service") {
			continue
		}
		output, e := ctl(ctx, "show", unit, "--property=Id,ExecStart")
		if e != nil {
			return e
		}
		canonicalUnit := unit
		for _, property := range strings.Split(output, "\n") {
			if id, ok := strings.CutPrefix(property, "Id="); ok && id != "" {
				canonicalUnit = id
			}
		}
		for _, m := range pathRE.FindAllStringSubmatch(output, -1) {
			p, e := resolve(m[1])
			if e == nil && p == BinaryPath {
				if canonicalUnit != "mi-node.service" {
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
	if err := ensureUnits(ctx, "/etc/systemd/system", map[string]string{"mi-node-update.service": ServiceUnit, "mi-node-update.timer": TimerUnit}, Systemctl); err != nil {
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
