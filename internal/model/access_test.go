package model

import (
	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/panel"
	"testing"
)

func TestAccessConfigRoundTripOwnsValues(t *testing.T) {
	original := &panel.NodeConfig{AccessLog: &access.Config{Enabled: true, BucketSeconds: 300, MaxEntries: 2500}}
	spec := NodeSpecFromPanel(original)
	if spec.AccessLog == nil || *spec.AccessLog != *original.AccessLog {
		t.Fatal("access config was not normalized")
	}
	original.AccessLog.Enabled = false
	if !spec.AccessLog.Enabled {
		t.Fatal("panel config mutation changed runtime capture")
	}
	back := spec.ToPanel()
	if back.AccessLog == nil || *back.AccessLog != *spec.AccessLog {
		t.Fatal("access config was not preserved")
	}
	back.AccessLog.BucketSeconds = 600
	if spec.AccessLog.BucketSeconds != 300 {
		t.Fatal("wire config aliases runtime config")
	}
	if NodeSpecFromPanel(&panel.NodeConfig{}).AccessLog != nil {
		t.Fatal("legacy node enabled capture")
	}
}
