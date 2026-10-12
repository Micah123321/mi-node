package updateagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/discovery"
)

func legacyBinding(url, token string, ids ...int) *config.Config {
	c := &config.Config{Panel: config.PanelConfig{URL: url, Token: token, NodeType: "vless"}}
	for _, id := range ids {
		c.Nodes = append(c.Nodes, config.NodeEntry{NodeID: id, NodeType: "vless"})
	}
	return c
}

func TestSelectAutoAuthority(t *testing.T) {
	a, b := "https://a.example", "https://b.example"
	one := legacyBinding(a+"/", "server-fixture", 9, 2, 9)
	two := legacyBinding(a, "server-fixture", 5, 2)
	other := legacyBinding(b, "other-fixture", 4)
	authority, auth, err := SelectAutoAuthority([]*config.Config{legacyBinding("http://old.example", "old", 1), one, other, two}, "")
	if err != nil || authority != a || auth["kind"] != "legacy" || !reflect.DeepEqual(auth["node_ids"], []int{2, 5, 9}) {
		t.Fatalf("legacy scope: %s %v", authority, err)
	}
	authority, auth, err = SelectAutoAuthority([]*config.Config{one, other}, b)
	if err != nil || authority != b || !reflect.DeepEqual(auth["node_ids"], []int{4}) {
		t.Fatal("existing authority changed")
	}
	if _, _, err = SelectAutoAuthority([]*config.Config{one}, b); err == nil {
		t.Fatal("missing existing authority silently replaced")
	}
	machine := &config.Config{Panel: config.PanelConfig{URL: a}, Machine: &config.MachineConfig{MachineID: 42, Token: "machine-fixture"}}
	authority, auth, err = SelectAutoAuthority([]*config.Config{machine, other}, "")
	if err != nil || authority != a || auth["machine_id"] != 42 || auth["token"] != "machine-fixture" || auth["node_ids"] != nil {
		t.Fatal("incorrect machine scope")
	}
	cases := [][]*config.Config{
		{one, legacyBinding(a, "different-fixture", 7)},
		{one, machine},
		{machine, {Panel: config.PanelConfig{URL: a}, Machine: &config.MachineConfig{MachineID: 43, Token: "machine-fixture"}}},
		{machine, {Panel: config.PanelConfig{URL: a}, Machine: &config.MachineConfig{MachineID: 42, Token: "different-fixture"}}},
		{legacyBinding("https://bad.example?token=fixture", "fixture", 1)},
	}
	for n, bindings := range cases {
		if _, _, err := SelectAutoAuthority(bindings, ""); err == nil {
			t.Errorf("conflicting binding %d accepted", n)
		}
	}
}

func autoFixture(t *testing.T, url string) autoInstaller {
	t.Helper()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("private root-owned enrollment state requires Linux root")
	}
	return autoInstaller{directory: t.TempDir(), identity: func() (string, error) { return "fixture-machine-identity", nil },
		loadRoot: func(string) (*config.RootConfig, error) {
			return &config.RootConfig{Config: *legacyBinding(url, "server-fixture", 9, 2, 9)}, nil
		}, client: NewClient}
}

func TestAutoEnrollmentLostACKStableIdentityAndNoTicket(t *testing.T) {
	var requests [][]byte
	var dir string
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != endpoint+"/auto-enroll" || r.Method != "POST" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" || r.Header.Get("Content-Type") != "application/json" {
			t.Error("credential escaped JSON POST contract")
		}
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
			return
		}
		if len(body) != 4 || body["enrollment_id"] != nil || body["enrollment_secret"] != nil {
			t.Error("automatic enrollment used a ticket")
		}
		var auth map[string]any
		_ = json.Unmarshal(body["auth"], &auth)
		if string(body["protocol_version"]) != "1" || auth["kind"] != "legacy" || auth["token"] != "server-fixture" || !reflect.DeepEqual(auth["node_ids"], []any{float64(2), float64(9)}) {
			t.Error("legacy authentication scope differs from local bindings")
		}
		encoded, _ := json.Marshal(body)
		requests = append(requests, encoded)
		var saved Config
		if err := readJSON(filepath.Join(dir, "config.json"), &saved); err != nil {
			t.Error(err)
		}
		var id, token string
		_ = json.Unmarshal(body["installation_id"], &id)
		_ = json.Unmarshal(body["agent_token"], &token)
		if saved.InstallationID != id || saved.Token != token || len(token) != 43 || saved.Enrolled {
			t.Error("identity not durable before request")
		}
		if len(requests) == 1 {
			contractDropACK(t, w)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"installation_id": id, "scope": json.RawMessage(`{"kind":"legacy","node_ids":[2,9]}`), "policy_revision": 1}})
	}))
	defer srv.Close()
	i := autoFixture(t, srv.URL)
	dir = i.directory
	inv, err := discovery.Load(dir, "v1.2.3")
	if err != nil {
		t.Fatal(err)
	}
	i.client = func(authority, token string) (*Client, error) {
		c, err := NewClient(authority, token)
		if err == nil {
			c.HTTP = srv.Client()
		}
		return c, err
	}
	c, err := i.enroll(context.Background(), "/fixture/config.yml")
	if err == nil || c.InstallationID != inv.InstallationID {
		t.Fatal("lost ACK should preserve discovery identity")
	}
	info, _ := os.Stat(filepath.Join(dir, "config.json"))
	if info == nil || info.Mode().Perm() != 0600 {
		t.Fatal("enrollment config permissions")
	}
	fresh := i
	c, err = fresh.enroll(context.Background(), "/fixture/config.yml")
	if err != nil || !c.Enrolled || c.PolicyRevision != 1 {
		t.Fatalf("retry: %v", err)
	}
	if len(requests) != 2 || !bytes.Equal(requests[0], requests[1]) {
		t.Fatal("lost ACK retry changed identity/token/scope")
	}
	if _, err = fresh.enroll(context.Background(), "/fixture/config.yml"); err != nil || len(requests) != 2 {
		t.Fatal("registered installation enrolled again")
	}
}

func savedAutoFixture(t *testing.T, i autoInstaller) Config {
	t.Helper()
	c := Config{InstallationID: UUID(), Authority: "https://original.example", Token: strings.Repeat("A", 43), MachineIdentity: "fixture-machine-identity", Scope: json.RawMessage(`{"kind":"legacy","node_ids":[2,9]}`), Enrolled: true, EnrollmentID: UUID()}
	if err := AtomicJSON(filepath.Join(i.directory, "config.json"), c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestAutoInstallRetainsManualIdentityAndDisabledAndUnlocksBeforeRun(t *testing.T) {
	for _, disabled := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual", true: "disabled"}[disabled], func(t *testing.T) {
			i := autoFixture(t, "https://other.example")
			c := savedAutoFixture(t, i)
			c.Disabled = disabled
			if err := AtomicJSON(filepath.Join(i.directory, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(filepath.Join(i.directory, "config.json"))
			i.loadRoot = func(string) (*config.RootConfig, error) {
				t.Fatal("enrolled identity consulted other authority")
				return nil, nil
			}
			i.client = func(string, string) (*Client, error) { t.Fatal("manual identity auto-enrolled"); return nil, nil }
			locked, ran, units, finished := false, false, false, false
			i.lock = func() (func(), error) { locked = true; return func() { locked = false }, nil }
			i.checkLayout = func(context.Context) error { return nil }
			i.installUnits = func(context.Context) error { units = true; return nil }
			i.finishBootstrap = func(context.Context) error { finished = true; return nil }
			i.run = func(_ context.Context, got Config) error {
				if locked {
					t.Fatal("Agent.Run invoked while enrollment lock held")
				}
				if !reflect.DeepEqual(got, c) {
					t.Fatal("existing identity changed")
				}
				ran = true
				return nil
			}
			if err := i.install(context.Background(), "/other/config.yml"); err != nil {
				t.Fatal(err)
			}
			after, _ := os.ReadFile(filepath.Join(i.directory, "config.json"))
			if !bytes.Equal(before, after) || !finished || ran == disabled || units == disabled {
				t.Fatal("identity/disabled/installation behavior changed")
			}
		})
	}
}

func TestAutoEnrollmentNeverRebuildsBadCopiedOrManualConfig(t *testing.T) {
	for _, kind := range []string{"broken", "copied", "invalid", "manual-pending", "scope-changed"} {
		t.Run(kind, func(t *testing.T) {
			i := autoFixture(t, "https://original.example")
			c := savedAutoFixture(t, i)
			switch kind {
			case "copied":
				c.MachineIdentity = "another-machine"
			case "invalid":
				c.Token = "broken"
			case "manual-pending":
				c.Enrolled = false
			case "scope-changed":
				c.Enrolled, c.AutoEnroll, c.EnrollmentID, c.AutoScope = false, true, "", json.RawMessage(`{"kind":"legacy","node_ids":[99]}`)
			}
			if err := AtomicJSON(filepath.Join(i.directory, "config.json"), c); err != nil {
				t.Fatal(err)
			}
			if kind == "broken" {
				if err := os.WriteFile(filepath.Join(i.directory, "config.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			before, _ := os.ReadFile(filepath.Join(i.directory, "config.json"))
			i.client = func(string, string) (*Client, error) {
				t.Fatal("bad installation contacted enrollment endpoint")
				return nil, nil
			}
			if _, err := i.enroll(context.Background(), "/fixture/config.yml"); err == nil {
				t.Fatal("invalid installation accepted")
			}
			after, _ := os.ReadFile(filepath.Join(i.directory, "config.json"))
			if !bytes.Equal(before, after) {
				t.Fatal("existing config was rebuilt")
			}
		})
	}
}

func TestAutoInstallNetworkFailureLeavesIndependentRetry(t *testing.T) {
	i := autoFixture(t, "https://original.example")
	locked, finished, units, ran := false, false, false, false
	i.checkLayout = func(context.Context) error { return nil }
	i.lock = func() (func(), error) { locked = true; return func() { locked = false }, nil }
	i.client = func(string, string) (*Client, error) { return nil, errors.New("network fixture unavailable") }
	i.finishBootstrap = func(context.Context) error { finished = true; return nil }
	i.installUnits = func(context.Context) error { units = true; return nil }
	i.run = func(context.Context, Config) error { ran = true; return nil }
	if err := i.install(context.Background(), "/fixture/config.yml"); err == nil || locked || finished || units || ran {
		t.Fatal("failed enrollment stopped retries or retained lock")
	}
	c, err := loadConfigAt(i.directory, i.identity)
	if err != nil || c.Enrolled || len(c.Token) != 43 {
		t.Fatal("retry identity missing")
	}
}

func TestAutoEnrollmentMachineConflictsAndRevocationRetainIdentity(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusConflict, http.StatusGone} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var seen []Config
			srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != endpoint+"/auto-enroll" || r.URL.RawQuery != "" || r.Header.Get("Authorization") != "" {
					t.Error("machine enrollment transport changed")
				}
				var body struct {
					Protocol int    `json:"protocol_version"`
					ID       string `json:"installation_id"`
					Token    string `json:"agent_token"`
					Auth     struct {
						Kind      string `json:"kind"`
						MachineID int    `json:"machine_id"`
						Token     string `json:"token"`
						NodeIDs   []int  `json:"node_ids"`
					} `json:"auth"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body.Protocol != 1 || body.Auth.Kind != "machine" || body.Auth.MachineID != 42 || body.Auth.Token != "machine-fixture" || body.Auth.NodeIDs != nil {
					t.Error("machine authentication scope changed")
				}
				seen = append(seen, Config{InstallationID: body.ID, Token: body.Token})
				w.WriteHeader(status)
				if status != http.StatusOK {
					_, _ = w.Write([]byte(`{"error":{"code":"installation_revoked"}}`))
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"installation_id": body.ID, "scope": json.RawMessage(`{"kind":"machine","machine_id":42}`), "policy_revision": 1}})
			}))
			defer srv.Close()
			i := autoFixture(t, srv.URL)
			i.loadRoot = func(string) (*config.RootConfig, error) {
				return &config.RootConfig{Config: config.Config{Panel: config.PanelConfig{URL: srv.URL}, Machine: &config.MachineConfig{MachineID: 42, Token: "machine-fixture"}}}, nil
			}
			i.client = func(authority, token string) (*Client, error) {
				c, err := NewClient(authority, token)
				if err == nil {
					c.HTTP = srv.Client()
				}
				return c, err
			}
			for range 2 {
				c, err := i.enroll(context.Background(), "/fixture/machine.yml")
				if (err == nil) != (status == http.StatusOK) || c.Enrolled != (status == http.StatusOK) {
					t.Fatalf("machine status %d: %v", status, err)
				}
			}
			if status == http.StatusOK {
				if len(seen) != 1 {
					t.Fatal("registered machine enrolled again")
				}
			} else if len(seen) != 2 || !reflect.DeepEqual(seen[0], seen[1]) {
				t.Fatal("conflict/revocation rebuilt identity")
			}
		})
	}
}
