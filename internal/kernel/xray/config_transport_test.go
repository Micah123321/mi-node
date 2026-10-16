package xray

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/panel"
	"github.com/xtls/xray-core/app/proxyman"
	"github.com/xtls/xray-core/infra/conf/serial"
	"github.com/xtls/xray-core/transport/internet"
	"github.com/xtls/xray-core/transport/internet/finalmask/mkcp/aes128gcm"
	"github.com/xtls/xray-core/transport/internet/headers/http"
	"github.com/xtls/xray-core/transport/internet/tcp"
)

func panelTransportConfig(t *testing.T, network string, settings M) (M, []byte) {
	t.Helper()
	kcfg := testKernelCfg
	disabled := false
	kcfg.Egress.EnableDefaultRules = &disabled
	node := testNodeSpec(&panel.NodeConfig{
		Protocol: "vmess", ServerPort: 10086, Network: network, NetworkSettings: settings,
	})
	cfg := buildConfig(kcfg, node, testUsers, kernel.TLSCert{})
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return cfg["inbounds"].([]M)[0]["streamSettings"].(M), data
}

func parsePanelTransport(t *testing.T, data []byte) *internet.StreamConfig {
	t.Helper()
	cfg, err := serial.LoadJSONConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("Xray rejected generated config: %v", err)
	}
	receiver, err := cfg.Inbound[0].ReceiverSettings.GetInstance()
	if err != nil {
		t.Fatal(err)
	}
	return receiver.(*proxyman.ReceiverConfig).StreamSettings
}

func TestBuildConfig_TCPHTTPHeader(t *testing.T) {
	for _, host := range []string{
		`["one.example", "two.example"]`,
		`"one.example"`,
	} {
		t.Run(host, func(t *testing.T) {
			var settings M
			if err := json.Unmarshal([]byte(`{"header":{"type":"http","request":{"version":"1.1","method":"GET","path":["/tunnel","/fallback"],"headers":{"Host":`+host+`,"User-Agent":["test-agent"]}},"response":{"status":"204","reason":"No Content"}}}`), &settings); err != nil {
				t.Fatal(err)
			}
			_, data := panelTransportConfig(t, "tcp", settings)
			stream := parsePanelTransport(t, data)
			if stream.ProtocolName != "tcp" || len(stream.TransportSettings) != 1 {
				t.Fatalf("unexpected TCP stream: %v", stream)
			}
			raw, err := stream.TransportSettings[0].Settings.GetInstance()
			if err != nil {
				t.Fatal(err)
			}
			header, err := raw.(*tcp.Config).HeaderSettings.GetInstance()
			if err != nil {
				t.Fatal(err)
			}
			httpConfig := header.(*http.Config)
			if !reflect.DeepEqual(httpConfig.Request.Uri, []string{"/tunnel", "/fallback"}) || httpConfig.Request.Method.Value != "GET" || httpConfig.Request.Version.Value != "1.1" {
				t.Fatalf("HTTP request changed: %v", httpConfig.Request)
			}
			wantHosts := []string{"one.example"}
			if strings.HasPrefix(host, "[") {
				wantHosts = append(wantHosts, "two.example")
			}
			if !reflect.DeepEqual(httpConfig.Request.Header[0].Value, wantHosts) || httpConfig.Request.Header[0].Name != "Host" {
				t.Fatalf("HTTP Host changed: %v", httpConfig.Request.Header)
			}
			if httpConfig.Response.Status.Code != "204" || httpConfig.Response.Status.Reason != "No Content" {
				t.Fatalf("HTTP response changed: %v", httpConfig.Response)
			}
		})
	}
	for _, network := range []string{"", "tcp"} {
		stream, data := panelTransportConfig(t, network, nil)
		if _, ok := stream["tcpSettings"]; ok {
			t.Fatal("plain TCP acquired header settings")
		}
		parsePanelTransport(t, data)
	}
	_, data := panelTransportConfig(t, "tcp", M{"header": M{"type": "none"}})
	parsePanelTransport(t, data)
}

func TestBuildConfig_KCPMasks(t *testing.T) {
	for _, network := range []string{"kcp", "mkcp"} {
		for _, header := range []string{"none", "srtp", "utp", "wechat-video", "dtls", "wireguard"} {
			for _, seed := range []string{"", "panel-seed"} {
				t.Run(network+"/"+header+"/"+seed, func(t *testing.T) {
					settings := M{"seed": seed, "header": M{"type": header}, "mtu": 1350, "tti": 20}
					stream, data := panelTransportConfig(t, network, settings)
					if _, ok := stream["kcpSettings"].(M)["header"]; ok {
						t.Fatal("removed KCP header field emitted")
					}
					if _, ok := stream["kcpSettings"].(M)["seed"]; ok {
						t.Fatal("removed KCP seed field emitted")
					}
					parsed := parsePanelTransport(t, data)
					if parsed.ProtocolName != "mkcp" || len(parsed.TransportSettings) != 1 {
						t.Fatalf("unexpected mKCP stream: %v", parsed)
					}
					masks := stream["finalmask"].(M)["udp"].([]M)
					wantMaskCount := 1
					if header != "none" {
						wantMaskCount++
						kind := header
						if kind == "wechat-video" {
							kind = "wechat"
						}
						if masks[0]["type"] != "header-"+kind {
							t.Fatalf("unexpected header mask: %v", masks)
						}
					}
					if len(parsed.Udpmasks) != wantMaskCount {
						t.Fatalf("got %d masks, want %d", len(parsed.Udpmasks), wantMaskCount)
					}
					last := masks[len(masks)-1]
					if seed == "" {
						if last["type"] != "mkcp-original" {
							t.Fatalf("missing original encryption: %v", last)
						}
					} else {
						mask, err := parsed.Udpmasks[len(parsed.Udpmasks)-1].GetInstance()
						if err != nil {
							t.Fatal(err)
						}
						if mask.(*aes128gcm.Config).Password != seed {
							t.Fatal("seed was not preserved in AES mask")
						}
					}
				})
			}
		}
		_, data := panelTransportConfig(t, network, nil)
		parsePanelTransport(t, data)
	}
}

func TestBuildConfig_H2HostsAndRemovedTransport(t *testing.T) {
	for _, network := range []string{"h2", "http"} {
		for _, host := range []any{"one.example", []string{"one.example", "two.example"}, []any{"one.example", "two.example"}} {
			stream, data := panelTransportConfig(t, network, M{"path": "/tunnel", "host": host})
			raw, err := json.Marshal(stream["httpSettings"])
			if err != nil {
				t.Fatal(err)
			}
			var settings struct {
				Host []string `json:"host"`
				Path string   `json:"path"`
			}
			if err := json.Unmarshal(raw, &settings); err != nil {
				t.Fatalf("H2 host is not a flat string array: %v", err)
			}
			want := []string{"one.example", "two.example"}
			if _, ok := host.(string); ok {
				want = want[:1]
			}
			if !reflect.DeepEqual(settings.Host, want) || settings.Path != "/tunnel" {
				t.Fatalf("H2 settings changed: %+v", settings)
			}
			// This dependency removed H2; do not silently substitute a different transport.
			if _, err := serial.LoadJSONConfig(bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "HTTP transport") {
				t.Fatalf("expected Xray's removed HTTP transport error, got %v", err)
			}
		}
	}
}
