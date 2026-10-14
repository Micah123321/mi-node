package kernel

import (
	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/model"
	"testing"
)

func TestAccessCaptureSettingsDoNotRestartListeners(t *testing.T) {
	original := &model.NodeSpec{Protocol: "vless", ServerPort: 443}
	updated := *original
	updated.AccessLog = &access.Config{Enabled: true, BucketSeconds: 600, MaxEntries: 5000}
	if ConfigHash(original) != ConfigHash(&updated) {
		t.Fatal("capture-only config change would restart active connections")
	}
	updated.ServerPort = 8443
	if ConfigHash(original) == ConfigHash(&updated) {
		t.Fatal("listener change must still rebuild the kernel")
	}
}
