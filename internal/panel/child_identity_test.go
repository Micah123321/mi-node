package panel

import (
	"context"
	"github.com/micah123321/mi-node/internal/config"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChildCapabilityAllPanelRequestBuilders(t *testing.T) {
	expected := ""
	if strconv.IntSize == 64 {
		expected = "1"
	}
	var calls atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if got := r.Header.Get("X-Mi-Node-Child-Rate"); got != expected {
			t.Errorf("%s %s: capability=%q", r.Method, r.URL.Path, got)
		}
		if r.URL.Path == "/ws" {
			http.Error(w, "handshake inspected", http.StatusBadRequest)
			return
		}
		w.Write([]byte(`{"data":{"enabled":false,"suspended":false,"block_service":false,"next_check_at":0}}`))
	}))
	defer server.Close()
	c := NewClient(config.PanelConfig{URL: server.URL, NodeID: 1})
	for _, client := range []*Client{c, c.ForNode(2)} {
		for _, method := range []string{"GET", "POST"} {
			resp, err := client.doRequest(method, "/test", nil, "")
			if err != nil {
				t.Fatal(err)
			}
			drainAndClose(resp.Body)
		}
	}
	bw := c.NewBandwidthClient()
	defer bw.Close()
	for _, operation := range []string{"config", "upload", "report"} {
		result := map[string]any{}
		if err := bw.request(context.Background(), "POST", operation, nil, nil, &result); err != nil {
			t.Fatal(err)
		}
	}
	ws := NewWSClient("ws"+strings.TrimPrefix(server.URL, "http")+"/ws", "fixture", 1, WSClientConfig{}, nil, nil, nil)
	if err := ws.connect(context.Background()); err == nil {
		t.Fatal("expected inspected handshake to end")
	}
	if calls.Load() != 8 {
		t.Fatalf("requests=%d", calls.Load())
	}
}
