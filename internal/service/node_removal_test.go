package service

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/panel"
)

type removalPush struct{}

func (*removalPush) Run(ctx context.Context)           { <-ctx.Done() }
func (*removalPush) IsConnected() bool                 { return true }
func (*removalPush) SendDeviceReport(map[int][]string) {}

type removalSource struct {
	controlplane.LocalControlPlane
	client *panel.Client
	push   controlplane.PushClient
	ready  chan struct{}
}

func (s *removalSource) CheckNode(ctx context.Context) error { return s.client.CheckNode(ctx) }
func (s *removalSource) SupportsPolling() bool               { return true }
func (s *removalSource) Initial(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	if s.ready != nil {
		close(s.ready)
	}
	return controlplane.Bootstrap{Config: &model.NodeSpec{Protocol: "shadowsocks", ServerPort: 12345}, Users: []model.UserSpec{}, Push: s.push}, nil
}

func TestRemovedNodeRetiresOnStartupAndWithConnectedWS(t *testing.T) {
	for _, deletedAtStart := range []bool{true, false} {
		t.Run(fmt.Sprint(deletedAtStart), func(t *testing.T) {
			var deleted atomic.Bool
			deleted.Store(deletedAtStart)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v2/server/bandwidth/config" {
					fmt.Fprint(w, `{"data":{"enabled":false,"suspended":false,"block_service":false,"next_check_at":0}}`)
					return
				}
				if deleted.Load() {
					w.WriteHeader(400)
					fmt.Fprint(w, `{"status":"fail","message":"Server does not exist"}`)
					return
				}
				fmt.Fprint(w, `{"websocket":{"enabled":true}}`)
			}))
			defer server.Close()
			cfg := &config.Config{Panel: config.PanelConfig{URL: server.URL, NodeID: 173, Token: "fixture"}, Kernel: config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir()}, Cert: config.CertConfig{CertMode: "none", CertDir: filepath.Join(t.TempDir(), "certs")}, Node: config.NodeConfig{PullInterval: 1}}
			cp := &removalSource{client: panel.NewClient(cfg.Panel), push: &removalPush{}, ready: make(chan struct{})}
			// This test exercises retirement; the independent bandwidth runner has up to 30s startup jitter.
			cfg.Panel.URL = ""
			svc := NewWithControlPlane(cfg, cp)
			ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- svc.Run(ctx) }()
			if !deletedAtStart {
				select {
				case <-cp.ready:
				case <-ctx.Done():
					t.Fatal("service never initialized")
				}
				deleted.Store(true)
			}
			select {
			case err := <-result:
				if !panel.IsNodeRemoved(err) {
					t.Fatalf("expected node retirement: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("online WS concealed node deletion")
			}
			if svc.kernel.IsRunning() {
				t.Fatal("deleted node still listening")
			}
		})
	}
}
