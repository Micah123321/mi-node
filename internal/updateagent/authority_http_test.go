package updateagent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
)

func TestPanelAuthorityHTTPRequestsAndArtifactHTTPSValidationRemainSeparate(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" || r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			t.Error("panel credential transport contract changed")
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if strings.HasSuffix(r.URL.Path, "/auto-enroll") {
			if r.Header.Get("Authorization") != "" || body["auth"].(map[string]any)["token"] != "existing-fixture" {
				t.Error("existing binding authentication missing")
			}
		} else if r.Header.Get("Authorization") != "Bearer agent-fixture" {
			t.Error("agent authentication missing")
		}
		paths = append(paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"data\":{\"accepted\":true}}"))
	}))
	defer server.Close()
	registration, err := NewClient(server.URL, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := registration.Post(context.Background(), "/auto-enroll", map[string]any{"auth": map[string]any{"token": "existing-fixture"}}, nil); err != nil {
		t.Fatal(err)
	}
	agent, err := NewClient(server.URL, "agent-fixture")
	if err != nil {
		t.Fatal(err)
	}
	if err := agent.Post(context.Background(), "/poll", map[string]any{"installation_id": "fixture"}, nil); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(paths, []string{endpoint + "/auto-enroll", endpoint + "/poll"}) {
		t.Fatalf("requests: %v", paths)
	}
	if err := validHTTPS(server.URL + "/mi-node"); err == nil {
		t.Fatal("HTTP panel compatibility changed artifact HTTPS requirement")
	}
	for _, invalid := range []string{"ftp://panel.example", "file:///etc/config", "https://user:private@panel.example", "http://panel.example?token=private", "http://panel.example#private", "http://panel.example?"} {
		if _, err := NewClient(invalid, ""); err == nil {
			t.Fatal("invalid panel URL accepted")
		}
	}
}

func TestHTTPAuthorityUsesExistingBindingAndRetainsAuthority(t *testing.T) {
	httpAuthority := "http://panel.example:8080"
	binding := legacyBinding(httpAuthority, "fixture-existing-token", 9, 2)
	authority, auth, err := SelectAutoAuthority([]*config.Config{binding}, "")
	if err != nil || authority != httpAuthority || !reflect.DeepEqual(auth["node_ids"], []int{2, 9}) {
		t.Fatalf("HTTP binding not reused: %s %v", authority, err)
	}
	secure := legacyBinding("https://other.example", "other-fixture", 4)
	authority, _, err = SelectAutoAuthority([]*config.Config{secure, binding}, httpAuthority)
	if err != nil || authority != httpAuthority {
		t.Fatal("existing HTTP identity moved to another panel")
	}
}

func TestAutoEnrollmentCompletesOverExistingHTTPBinding(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		auth, _ := body["auth"].(map[string]any)
		if r.URL.Path != endpoint+"/auto-enroll" || r.URL.RawQuery != "" || auth["kind"] != "legacy" || auth["token"] != "server-fixture" || !reflect.DeepEqual(auth["node_ids"], []any{float64(2), float64(9)}) {
			t.Error("existing HTTP binding not preserved")
		}
		requests++
		_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"installation_id": body["installation_id"], "scope": map[string]any{"kind": "legacy", "node_ids": []int{2, 9}}, "policy_revision": 1}})
	}))
	defer server.Close()
	installer := autoFixture(t, server.URL)
	saved, err := installer.enroll(context.Background(), "/fixture/config.yml")
	if err != nil || !saved.Enrolled || saved.Authority != server.URL {
		t.Fatalf("automatic HTTP enrollment failed: %v", err)
	}
	repeat, err := installer.enroll(context.Background(), "/fixture/config.yml")
	if err != nil || repeat.InstallationID != saved.InstallationID || repeat.Token != saved.Token || requests != 1 {
		t.Fatal("registered HTTP identity changed or enrolled twice")
	}
}

func TestPanelClientDoesNotFollowCredentialRedirect(t *testing.T) {
	destinationCalls := 0
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { destinationCalls++; w.WriteHeader(http.StatusOK) }))
	defer destination.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL+"/poll", http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	client, err := NewClient(source.URL, "fixture-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Post(context.Background(), "/poll", map[string]any{"installation_id": "fixture"}, nil); err == nil || destinationCalls != 0 {
		t.Fatalf("redirect followed: %v, calls=%d", err, destinationCalls)
	}
}
