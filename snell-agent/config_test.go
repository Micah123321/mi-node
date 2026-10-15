package agent

import "testing"

func TestPanelURLRequiresTLSOutsideLoopback(t *testing.T) {
	for _, tt := range []struct {
		url string
		ok  bool
	}{
		{"https://panel.example.test", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"http://LOCALHOST:8080", true},
		{"http://panel.example.test", false},
		{"http://localhost.example.test", false},
		{"http://127.0.0.1.example.test", false},
		{"http://0.0.0.0:8080", false},
		{"http://192.168.1.2", false},
	} {
		t.Run(tt.url, func(t *testing.T) {
			cfg := Config{PanelURL: tt.url, Token: "fixture", NodeID: 1}
			if err := cfg.normalize(); (err == nil) != tt.ok {
				t.Fatalf("normalize returned %v; accepted=%v", err, tt.ok)
			}
		})
	}
}
