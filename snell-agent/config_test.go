package agent

import "testing"

func TestPanelURLSupportsExplicitHTTPAndHTTPS(t *testing.T) {
	for _, tt := range []struct {
		url string
		ok  bool
	}{
		{"https://panel.example.test", true},
		{"http://127.0.0.1:8080", true},
		{"http://[::1]:8080", true},
		{"http://LOCALHOST:8080", true},
		{"http://panel.example.test", true},
		{"http://192.0.2.5:7001", true},
		{"https://panel.example.test/base", true},
		{"ftp://panel.example.test", false},
		{"https://user:fixture@panel.example.test", false},
		{"https://panel.example.test?token=fixture", false},
		{"https://panel.example.test#fragment", false},
	} {
		t.Run(tt.url, func(t *testing.T) {
			cfg := Config{PanelURL: tt.url, Token: "fixture", NodeID: 1}
			if err := cfg.normalize(); (err == nil) != tt.ok {
				t.Fatalf("normalize returned %v; accepted=%v", err, tt.ok)
			}
		})
	}
}
