package kernel

import (
	"testing"

	"github.com/micah123321/mi-node/internal/model"
)

func TestConfigHashesIgnoreTrafficQuota(t *testing.T) {
	original := &model.NodeSpec{Protocol: "hysteria", Version: 2, ServerPort: 42618,
		TrafficLimit: &model.TrafficLimitSpec{Enabled: true, Limit: 1000, CurrentUsed: 10}}
	users := []model.UserSpec{{ID: 1, UUID: "fixture-user"}}
	configHash := ConfigHash(original)
	kernelHash := ComputeHash(original, users)
	updated := *original
	updated.TrafficLimit = &model.TrafficLimitSpec{Enabled: true, Limit: 2000, CurrentUsed: 900,
		LastResetAt: 100, NextResetAt: 200, SuspendedAt: 150, Status: "suspended"}
	if ConfigHash(&updated) != configHash || ComputeHash(&updated, users) != kernelHash {
		t.Fatal("quota changes must not rebuild protocol listeners")
	}
	if original.TrafficLimit == nil || original.TrafficLimit.CurrentUsed != 10 {
		t.Fatal("hashing mutated the node quota state")
	}
	updated.TrafficLimit = nil
	if ConfigHash(&updated) != configHash {
		t.Fatal("disabling quotas must not rebuild protocol listeners")
	}
	updated.ServerPort++
	if ConfigHash(&updated) == configHash || ComputeHash(&updated, users) == kernelHash {
		t.Fatal("listening port changes must still invalidate both hashes")
	}
	updated.ServerPort = original.ServerPort
	if ComputeHash(&updated, []model.UserSpec{{ID: 1, UUID: "new-fixture-user"}}) == kernelHash {
		t.Fatal("user identity changes must still invalidate the kernel hash")
	}
}
