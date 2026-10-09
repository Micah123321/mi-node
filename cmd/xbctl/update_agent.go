package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/updateagent"
)

func runUpdateAgent(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: xbctl update-agent install|run|status|disable")
	}
	if err := ensureRoot("update-agent"); err != nil {
		return err
	}
	ctx := context.Background()
	if args[0] == "install" {
		fs := flag.NewFlagSet("update-agent install", flag.ContinueOnError)
		authority := fs.String("authority-panel-url", "", "HTTPS authority")
		file := fs.String("enrollment-file", "", "root 0600 enrollment file")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if fs.NArg() != 0 || *authority == "" || *file == "" {
			return fmt.Errorf("authority-panel-url and enrollment-file required")
		}
		reason, _, _ := updateagent.Capability()
		if reason != "" {
			return fmt.Errorf("automatic update unsupported: %s", reason)
		}
		unlock, err := updateagent.Lock(defaultBinaryPath)
		if err != nil {
			return err
		}
		defer unlock()
		if err = updateagent.CheckLayout(ctx); err != nil {
			return err
		}
		ticket, err := updateagent.ReadEnrollment(*file)
		if err != nil {
			return err
		}
		auth, err := enrollmentAuth(ticket.InstanceID, *authority)
		if err != nil {
			return err
		}
		c, err := updateagent.Enroll(ctx, *authority, ticket, auth)
		if err != nil {
			return err
		}
		if c.Disabled {
			return fmt.Errorf("installation locally disabled; explicit local re-enrollment procedure required")
		}
		return updateagent.InstallUnits(ctx)
	}
	if len(args) != 1 {
		return fmt.Errorf("unexpected update-agent arguments")
	}
	c, err := updateagent.LoadConfig()
	if err != nil {
		return err
	}
	switch args[0] {
	case "status":
		return updateagent.Status(c)
	case "disable":
		return updateagent.Disable(ctx, c)
	case "run":
		a, err := updateagent.NewAgent(c)
		if err != nil {
			return err
		}
		reason, init, container := updateagent.Capability()
		if reason == "" {
			if err = updateagent.CheckLayout(ctx); err != nil {
				reason = "shared_binary_layout"
			}
		}
		return a.Run(ctx, reason, init, container)
	default:
		return fmt.Errorf("unknown update-agent command")
	}
}
func enrollmentAuth(id, authority string) (map[string]any, error) {
	root, err := config.LoadRoot(defaultConfigPath)
	if err != nil {
		return nil, err
	}
	instances, err := root.NormalizeInstances()
	if err != nil {
		return nil, err
	}
	for _, c := range instances {
		if c.InstanceID != id {
			continue
		}
		if strings.TrimRight(c.Panel.URL, "/") != strings.TrimRight(authority, "/") {
			return nil, fmt.Errorf("binding is not on authority panel")
		}
		token, key := c.Panel.Token, c.Panel.TokenEnv
		auth := map[string]any{"kind": "legacy"}
		if c.IsMachineMode() {
			token, key = c.Machine.Token, c.Machine.TokenEnv
			auth["kind"] = "machine"
			auth["machine_id"] = c.Machine.MachineID
		}
		if token == "" && key != "" {
			data, e := os.ReadFile(defaultCredentialsPath)
			if e != nil {
				return nil, fmt.Errorf("read local credentials failed")
			}
			for _, line := range strings.Split(string(data), "\n") {
				k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
				if ok && k == key {
					token = strings.Trim(v, "\"'")
					break
				}
			}
		}
		if token == "" {
			return nil, fmt.Errorf("binding credential missing")
		}
		auth["token"] = token
		return auth, nil
	}
	return nil, fmt.Errorf("enrollment instance_id does not reference a local binding")
}
