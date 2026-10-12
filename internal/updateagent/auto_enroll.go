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
	"reflect"
	"sort"
	"strings"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/discovery"
)

// SelectAutoAuthority gathers one complete scope. A machine credential cannot
// authenticate other machines or legacy nodes on the same panel.
func SelectAutoAuthority(instances []*config.Config, authority string) (string, map[string]any, error) {
	if authority != "" {
		client, err := NewClient(authority, "")
		if err != nil {
			return "", nil, enrollmentError("authority_unavailable", err)
		}
		authority = client.Authority
	} else {
		// Preserve HTTPS preference while accepting existing HTTP-only installations.
		for _, scheme := range []string{"https://", "http://"} {
			for _, c := range instances {
				if c == nil || c.Standalone != nil {
					continue
				}
				if client, err := NewClient(c.Panel.URL, ""); err == nil && strings.HasPrefix(client.Authority, scheme) {
					authority = client.Authority
					break
				}
			}
			if authority != "" {
				break
			}
		}
	}
	if authority == "" {
		return "", nil, enrollmentError("authority_unavailable", fmt.Errorf("no usable HTTP/HTTPS authority binding"))
	}
	auth := map[string]any{}
	nodes := map[int]bool{}
	for _, c := range instances {
		if c == nil || c.Standalone != nil {
			continue
		}
		client, err := NewClient(c.Panel.URL, "")
		if err != nil || client.Authority != authority {
			continue
		}
		kind, token, machineID := "legacy", c.Panel.Token, 0
		if c.IsMachineMode() {
			kind, token, machineID = "machine", c.Machine.Token, c.Machine.MachineID
		}
		if token == "" {
			return "", nil, enrollmentError("credential_missing", fmt.Errorf("authority credential missing"))
		}
		if len(auth) == 0 {
			auth["kind"], auth["token"] = kind, token
			if kind == "machine" {
				auth["machine_id"] = machineID
			}
		} else if auth["kind"] != kind || auth["token"] != token || (kind == "machine" && auth["machine_id"] != machineID) {
			return "", nil, enrollmentError("scope_conflict", fmt.Errorf("authority bindings require incompatible credentials/scopes"))
		}
		if kind == "machine" {
			if machineID <= 0 {
				return "", nil, enrollmentError("validation_failed", fmt.Errorf("invalid authority machine"))
			}
			continue
		}
		for _, node := range c.ExpandNodes() {
			if node.Panel.NodeID <= 0 {
				return "", nil, enrollmentError("validation_failed", fmt.Errorf("invalid authority node"))
			}
			nodes[node.Panel.NodeID] = true
		}
	}
	if len(auth) == 0 {
		return "", nil, enrollmentError("authority_unavailable", fmt.Errorf("existing authority has no local binding"))
	}
	if auth["kind"] == "legacy" {
		ids := make([]int, 0, len(nodes))
		for id := range nodes {
			ids = append(ids, id)
		}
		sort.Ints(ids)
		auth["node_ids"] = ids
	}
	return authority, auth, nil
}

func autoScope(auth map[string]any) json.RawMessage {
	scope := map[string]any{"kind": auth["kind"]}
	if auth["kind"] == "machine" {
		scope["machine_id"] = auth["machine_id"]
	} else {
		scope["node_ids"] = auth["node_ids"]
	}
	b, _ := json.Marshal(scope)
	return b
}

func sameScope(a, b json.RawMessage) bool {
	var x, y any
	return json.Unmarshal(a, &x) == nil && json.Unmarshal(b, &y) == nil && reflect.DeepEqual(x, y)
}

type autoInstaller struct {
	directory       string
	identity        func() (string, error)
	loadRoot        func(string) (*config.RootConfig, error)
	client          func(string, string) (*Client, error)
	lock            func() (func(), error)
	checkLayout     func(context.Context) error
	installUnits    func(context.Context) error
	finishBootstrap func(context.Context) error
	run             func(context.Context, Config) error
	status          *enrollmentAttempt
}

// AutoInstall runs in the independent bootstrap service, never in mi-node.
func AutoInstall(ctx context.Context, configPath string) error {
	reason, init, container := Capability()
	if os.Geteuid() != 0 || reason != "" {
		status := newEnrollmentAttempt(StateDir, "bootstrap")
		status.state, status.code, status.preserveTerminal = "unsupported", "unsupported_platform", true
		status.finish(nil)
		return fmt.Errorf("automatic enrollment unsupported")
	}
	installer := autoInstaller{
		directory: StateDir, identity: machineIdentity, loadRoot: config.LoadRoot, client: NewClient,
		lock: func() (func(), error) { return Lock(BinaryPath) }, checkLayout: CheckLayout,
		installUnits: InstallUnits,
		finishBootstrap: func(ctx context.Context) error {
			_, err := Systemctl(ctx, "disable", "--now", bootstrapTimer)
			return err
		},
		run: func(ctx context.Context, c Config) error {
			a, err := NewAgent(c)
			if err != nil {
				return err
			}
			return a.Run(ctx, "", init, container)
		},
	}
	return installer.install(ctx, configPath)
}

func (i autoInstaller) install(ctx context.Context, configPath string) (result error) {
	status := newEnrollmentAttempt(i.directory, "layout")
	status.preserveTerminal = true
	i.status = status
	defer func() { status.finish(result) }()
	if err := i.checkLayout(ctx); err != nil {
		return err
	}
	status.stage = "lock"
	c, err := func() (c Config, result error) {
		unlock, err := i.lock()
		if err != nil {
			return c, enrollmentError("update_busy", err)
		}
		defer unlock()
		// Publish under enrollment ownership, before another installer can acquire it.
		defer func() { status.finish(result) }()
		status.preserveTerminal = false
		c, err = i.enroll(ctx, configPath)
		if err != nil {
			return c, err
		}
		status.stage = "units"
		if !c.Disabled {
			if err = i.installUnits(ctx); err != nil {
				return c, enrollmentError("systemd_failed", err)
			}
		}
		if err = i.finishBootstrap(ctx); err != nil {
			return c, enrollmentError("systemd_failed", err)
		}
		return c, nil
	}()
	if err != nil || c.Disabled {
		return err
	}
	// Agent.Run owns poll diagnostics; later task errors must not overwrite them.
	return i.run(ctx, c)
}

func (i autoInstaller) enroll(ctx context.Context, configPath string) (c Config, result error) {
	status := i.status
	if status == nil {
		status = newEnrollmentAttempt(i.directory, "config")
		defer func() { status.finish(result) }()
	}
	status.stage = "config"
	var err error
	c, err = loadConfigAt(i.directory, i.identity)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return c, enrollmentError("config_invalid", err)
	}
	if !missing {
		if c.Disabled {
			status.state, status.code = "disabled", "local_disabled"
			return c, nil
		}
		if c.Enrolled {
			status.state = "registered"
			return c, nil
		}
		if !c.AutoEnroll || c.EnrollmentID != "" {
			return c, enrollmentError("config_invalid", fmt.Errorf("pending manual enrollment retained"))
		}
	}
	root, err := i.loadRoot(configPath)
	if err != nil {
		return c, enrollmentError("config_invalid", fmt.Errorf("read automatic enrollment bindings failed"))
	}
	instances, err := root.NormalizeInstances()
	if err != nil {
		return c, enrollmentError("config_invalid", fmt.Errorf("normalize automatic enrollment bindings failed"))
	}
	status.stage = "authority"
	authority, auth, err := SelectAutoAuthority(instances, c.Authority)
	if err != nil {
		return c, err
	}
	scope := autoScope(auth)
	if missing {
		status.stage = "config"
		identity, err := i.identity()
		if err != nil {
			return c, err
		}
		inventory, err := discovery.Load(i.directory, "")
		if err != nil {
			return c, err
		}
		if inventory.InstallationID[14:15] != "4" || !strings.Contains("89abAB", inventory.InstallationID[19:20]) {
			return c, fmt.Errorf("discovery installation ID is not UUIDv4")
		}
		var token [32]byte
		if _, err := rand.Read(token[:]); err != nil {
			return c, err
		}
		c = Config{InstallationID: inventory.InstallationID, Authority: authority, Token: base64.RawURLEncoding.EncodeToString(token[:]), MachineIdentity: identity, AutoEnroll: true, AutoScope: scope}
		if err := os.Chmod(i.directory, 0700); err != nil {
			return c, err
		}
		if err := AtomicJSON(filepath.Join(i.directory, "config.json"), c); err != nil {
			return c, err
		}
	} else if !sameScope(c.AutoScope, scope) {
		return c, enrollmentError("scope_mismatch", fmt.Errorf("pending automatic enrollment scope changed; identity retained"))
	}
	status.stage = "authority"
	client, err := i.client(c.Authority, "")
	if err != nil {
		return c, enrollmentError("authority_unavailable", err)
	}
	var response struct {
		InstallationID string          `json:"installation_id"`
		Scope          json.RawMessage `json:"scope"`
		Revision       *int64          `json:"policy_revision"`
	}
	body := map[string]any{"protocol_version": 1, "installation_id": c.InstallationID, "agent_token": c.Token, "auth": auth}
	status.stage = "enroll"
	if err := client.Post(ctx, "/auto-enroll", body, &response); err != nil {
		return c, err
	}
	if response.InstallationID != c.InstallationID || len(response.Scope) == 0 || string(response.Scope) == "null" || response.Revision == nil || *response.Revision < 1 {
		return c, fmt.Errorf("invalid automatic enrollment response")
	}
	c.Scope, c.PolicyRevision, c.Enrolled = response.Scope, *response.Revision, true
	err = AtomicJSON(filepath.Join(i.directory, "config.json"), c)
	if err == nil {
		status.state = "registered"
	}
	return c, err
}
