package updateagent

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestServiceLayoutSkipsTemplatesChecksInstancesAndCanonicalAliases(t *testing.T) {
	for _, scenario := range []string{"normal", "shared-instance", "missing-main", "query-error"} {
		t.Run(scenario, func(t *testing.T) {
			queried := map[string]int{}
			ctl := func(_ context.Context, args ...string) (string, error) {
				switch args[0] {
				case "list-unit-files":
					return "getty@.service enabled\nmi-node@.service disabled\nmi-node.service enabled\nnode-alias.service alias\nmi-node-update-bootstrap.service static", nil
				case "list-units":
					return "getty@tty1.service loaded active running\nmi-node.service loaded active running\nmi-node@other.service loaded inactive dead", nil
				case "show":
					unit := args[1]
					queried[unit]++
					if strings.HasSuffix(unit, "@.service") {
						return "", errors.New("template has no instance")
					}
					if unit == "mi-node.service" || unit == "node-alias.service" {
						path := BinaryPath
						if scenario == "missing-main" {
							path = "/different/mi-node"
						}
						return "Id=mi-node.service\nExecStart={ path=" + path + " ; argv[]=mi-node }", nil
					}
					if unit == "mi-node@other.service" && scenario == "shared-instance" {
						return "Id=" + unit + "\nExecStart={ path=" + BinaryPath + " ; argv[]=mi-node }", nil
					}
					if unit == "getty@tty1.service" && scenario == "query-error" {
						return "", errors.New("genuine query failure")
					}
					return "Id=" + unit + "\nExecStart={ path=/usr/bin/true ; argv[]=true }", nil
				}
				return "", fmt.Errorf("unexpected command %v", args)
			}
			err := checkServiceLayout(context.Background(), ctl, func(path string) (string, error) { return path, nil })
			if scenario == "normal" && err != nil {
				t.Fatal(err)
			}
			if scenario == "shared-instance" && (err == nil || err.Error() != "shared_binary_layout") {
				t.Fatalf("shared running instance ignored: %v", err)
			}
			if scenario == "missing-main" && (err == nil || !strings.Contains(err.Error(), "fixed mi-node.service")) {
				t.Fatalf("missing primary unit ignored: %v", err)
			}
			if scenario == "query-error" && (err == nil || !strings.Contains(err.Error(), "genuine query failure")) {
				t.Fatalf("real query error swallowed: %v", err)
			}
			if queried["getty@.service"] != 0 || queried["mi-node@.service"] != 0 || queried["mi-node.service"] != 1 {
				t.Fatalf("incorrect unit enumeration: %v", queried)
			}
			if scenario != "query-error" && queried["getty@tty1.service"] != 1 {
				t.Fatal("actual template instance not checked")
			}
		})
	}
}

func TestNativeSystemdServiceEnumeration(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("native Linux systemd required")
	}
	if _, err := exec.LookPath("systemctl"); err != nil {
		t.Skip("systemctl unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Systemctl(ctx, "show", "--property=Version"); err != nil {
		t.Skip("systemd manager unavailable")
	}
	err := checkServiceLayout(ctx, Systemctl, filepath.EvalSymlinks)
	// A test host need not have mi-node installed; every real service query must still work.
	if err != nil && strings.Contains(err.Error(), "systemctl") {
		t.Fatalf("native service enumeration failed: %v", err)
	}
}
