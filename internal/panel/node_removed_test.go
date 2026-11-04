package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/micah123321/mi-node/internal/config"
)

func TestCheckNodeRequiresPreciseV2Confirmation(t *testing.T) {
	cases := []struct {
		name    string
		code    int
		body    string
		removed bool
	}{
		{"deleted", 400, `{"status":"fail","message":"Server does not exist","data":null,"error":null}`, true},
		{"route_missing", 404, "not found", false},
		{"gone", 410, "gone", false},
		{"bad_token", 422, `{"message":"Invalid token"}`, false},
		{"disabled", 403, `{"message":"Machine is disabled"}`, false},
		{"html", 400, "<p>Server does not exist</p>", false},
		{"wrong_envelope", 400, `{"status":"success","message":"Server does not exist"}`, false},
		{"message_prefix", 400, `{"status":"fail","message":"Server does not exist temporarily"}`, false},
		{"present", 200, `{"websocket":{"enabled":true,"ws_url":"ws://fixture"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != "POST" || r.URL.Path != "/api/v2/server/handshake" {
					t.Error("wrong endpoint")
				}
				var auth map[string]any
				if err := json.NewDecoder(r.Body).Decode(&auth); err != nil {
					t.Error(err)
				}
				if auth["node_type"] != nil || auth["node_id"] != float64(173) || auth["token"] != "fixture" {
					t.Errorf("wrong identity fields")
				}
				w.WriteHeader(tc.code)
				fmt.Fprint(w, tc.body)
			}))
			defer ts.Close()
			client := NewClient(config.PanelConfig{URL: ts.URL, NodeID: 173, NodeType: "anytls", Token: "fixture"})
			err := client.CheckNode(context.Background())
			if IsNodeRemoved(err) != tc.removed {
				t.Fatalf("removed=%v error=%v", tc.removed, err)
			}
			expected := int32(1)
			if tc.removed {
				expected = 2
			}
			if calls.Load() != expected {
				t.Fatalf("calls=%d", calls.Load())
			}
		})
	}
}

func TestCheckNodeKeepsBindingIfConfirmationRecovers(t *testing.T) {
	var calls atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"status":"fail","message":"Server does not exist"}`)
			return
		}
		fmt.Fprint(w, `{"websocket":{"enabled":false}}`)
	}))
	defer ts.Close()
	if err := NewClient(config.PanelConfig{URL: ts.URL, NodeID: 173}).CheckNode(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestGetConfigNotFoundDoesNotRemoveBinding(t *testing.T) {
	ts, client := newTestServer(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(404) })
	defer ts.Close()
	_, err := client.GetConfig()
	if err == nil || IsNodeRemoved(err) {
		t.Fatalf("unconfirmed error: %v", err)
	}
}
