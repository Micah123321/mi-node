package trafficlimit

import (
	"path/filepath"
	"testing"
	"time"
)

func TestManagerSuspendsAndPersists(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "traffic-limit.json")
	manager := New(path, func() time.Time { return now })

	action, err := manager.Configure(Config{
		Enabled:   true,
		Limit:     100,
		ResetDay:  1,
		ResetTime: "00:00",
		Timezone:  "UTC",
	})
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}
	if action != ActionNone {
		t.Fatalf("Configure() action = %v, want ActionNone", action)
	}

	action, err = manager.AddTraffic(40, 61)
	if err != nil {
		t.Fatalf("AddTraffic() error = %v", err)
	}
	if action != ActionSuspend {
		t.Fatalf("AddTraffic() action = %v, want ActionSuspend", action)
	}
	if manager.CanRun() {
		t.Fatalf("CanRun() = true, want false")
	}

	restored := New(path, func() time.Time { return now })
	if action, err := restored.Configure(Config{Enabled: true, Limit: 100, ResetDay: 1, ResetTime: "00:00", Timezone: "UTC"}); err != nil || action != ActionNone {
		t.Fatalf("restored Configure() action = %v error = %v, want no transition without error", action, err)
	}
	if restored.CanRun() {
		t.Fatalf("restored CanRun() = true, want false")
	}
}

func TestManagerResetsWhenDue(t *testing.T) {
	now := time.Date(2026, 4, 1, 0, 0, 1, 0, time.UTC)
	manager := New(filepath.Join(t.TempDir(), "traffic-limit.json"), func() time.Time { return now })

	_, err := manager.Configure(Config{
		Enabled:     true,
		Limit:       100,
		ResetDay:    1,
		ResetTime:   "00:00",
		Timezone:    "UTC",
		CurrentUsed: 120,
		NextResetAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC).Unix(),
	})
	if err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	snapshot := manager.Snapshot()
	if snapshot.Suspended {
		t.Fatalf("Snapshot().Suspended = true, want false after due reset")
	}
	if snapshot.Used != 0 {
		t.Fatalf("Snapshot().Used = %d, want 0", snapshot.Used)
	}
	if snapshot.NextResetAt <= now.Unix() {
		t.Fatalf("NextResetAt = %d, want future timestamp", snapshot.NextResetAt)
	}
}

func TestManagerClampsMissingDayToEndOfMonth(t *testing.T) {
	now := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	manager := New("", func() time.Time { return now })

	if _, err := manager.Configure(Config{Enabled: true, Limit: 1, ResetDay: 31, ResetTime: "03:30", Timezone: "UTC"}); err != nil {
		t.Fatalf("Configure() error = %v", err)
	}

	got := time.Unix(manager.Snapshot().NextResetAt, 0).UTC()
	want := time.Date(2026, 2, 28, 3, 30, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("NextResetAt = %s, want %s", got, want)
	}
}

func TestManagerPanelResetResumes(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	manager := New("", func() time.Time { return now })

	if action, err := manager.Configure(Config{Enabled: true, Limit: 100, CurrentUsed: 120, ResetDay: 1, ResetTime: "00:00", Timezone: "UTC"}); err != nil || action != ActionSuspend {
		t.Fatalf("Configure() action = %v error = %v, want suspend without error", action, err)
	}

	action, err := manager.Configure(Config{
		Enabled:     true,
		Limit:       100,
		CurrentUsed: 0,
		LastResetAt: now.Unix() + 1,
		ResetDay:    1,
		ResetTime:   "00:00",
		Timezone:    "UTC",
	})
	if err != nil {
		t.Fatalf("Configure(reset) error = %v", err)
	}
	if action != ActionResume {
		t.Fatalf("Configure(reset) action = %v, want ActionResume", action)
	}
	if !manager.CanRun() {
		t.Fatalf("CanRun() = false, want true")
	}
}

func TestManagerLimitIncreaseResumesSuspendedNode(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	manager := New("", func() time.Time { return now })

	if action, err := manager.Configure(Config{Enabled: true, Limit: 100, CurrentUsed: 120, ResetDay: 1, ResetTime: "00:00", Timezone: "UTC"}); err != nil || action != ActionSuspend {
		t.Fatalf("Configure(initial) action = %v error = %v, want suspend without error", action, err)
	}
	if manager.CanRun() {
		t.Fatalf("CanRun() = true, want false before limit increase")
	}

	action, err := manager.Configure(Config{
		Enabled:     true,
		Limit:       500,
		CurrentUsed: 120,
		ResetDay:    1,
		ResetTime:   "00:00",
		Timezone:    "UTC",
	})
	if err != nil {
		t.Fatalf("Configure(increase) error = %v", err)
	}
	if action != ActionResume {
		t.Fatalf("Configure(increase) action = %v, want ActionResume", action)
	}
	if !manager.CanRun() {
		t.Fatalf("CanRun() = false, want true after limit increase")
	}

	snapshot := manager.Snapshot()
	if snapshot.Suspended {
		t.Fatalf("Snapshot().Suspended = true, want false")
	}
	if snapshot.Status != StatusNormal {
		t.Fatalf("Snapshot().Status = %q, want %q", snapshot.Status, StatusNormal)
	}
}

func TestManagerReconcilesSameCycleUsage(t *testing.T) {
	now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
	manager := New("", func() time.Time { return now })
	cfg := Config{Enabled: true, Limit: 100, CurrentUsed: 10, LastResetAt: now.Add(-time.Hour).Unix(),
		NextResetAt: now.Add(time.Hour).Unix(), Timezone: "UTC"}
	if _, err := manager.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	cfg.CurrentUsed = 100
	if action, err := manager.Configure(cfg); err != nil || action != ActionSuspend {
		t.Fatalf("same-cycle reconciliation: action=%v error=%v", action, err)
	}
	cfg.CurrentUsed = 10
	if action, err := manager.Configure(cfg); err != nil || action != ActionNone || manager.CanRun() {
		t.Fatal("a smaller report must not erase same-cycle usage or resume an exhausted quota")
	}
	cfg.LastResetAt = now.Unix()
	cfg.CurrentUsed = 0
	if action, err := manager.Configure(cfg); err != nil || action != ActionResume {
		t.Fatalf("explicit reset: action=%v error=%v", action, err)
	}
	cfg.LastResetAt--
	cfg.CurrentUsed = 100
	if action, err := manager.Configure(cfg); err != nil || action != ActionNone || manager.Snapshot().Used != 0 {
		t.Fatal("an old-cycle report must not refill a reset quota")
	}
}

func TestManagerDelayedScheduledResetReconcilesPanelCycle(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		day                    int
		due, checked, boundary time.Time
	}{
		{"late-check", 1, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 5, 1, 0, 0, 5, 0, time.UTC), time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)},
		{"missed-cycles", 1, time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 7, 13, 0, 0, 5, 0, time.UTC), time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)},
		{"short-month", 31, time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 28, 0, 0, 5, 0, time.UTC), time.Date(2026, 2, 28, 0, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := tc.due.Add(-time.Hour)
			path := filepath.Join(t.TempDir(), "quota.json")
			manager := New(path, func() time.Time { return now })
			cfg := Config{Enabled: true, Limit: 100, CurrentUsed: 100, ResetDay: tc.day, ResetTime: "00:00", Timezone: "UTC",
				LastResetAt: tc.due.Add(-24 * time.Hour).Unix(), NextResetAt: tc.due.Unix()}
			if action, err := manager.Configure(cfg); err != nil || action != ActionSuspend {
				t.Fatalf("initial quota: action=%v error=%v", action, err)
			}
			now = tc.checked
			if action, err := manager.CheckReset(); err != nil || action != ActionResume {
				t.Fatalf("scheduled reset: action=%v error=%v", action, err)
			}
			if got := manager.Snapshot().LastResetAt; got != tc.boundary.Unix() {
				t.Fatalf("reset time must identify the scheduled cycle: got=%d want=%d", got, tc.boundary.Unix())
			}
			if action, err := manager.AddTraffic(5, 0); err != nil || action != ActionNone {
				t.Fatalf("new-cycle local usage: action=%v error=%v", action, err)
			}
			restored := New(path, func() time.Time { return now })
			cfg.CurrentUsed = 200
			if action, err := restored.Configure(cfg); err != nil || action != ActionNone || restored.Snapshot().Used != 5 {
				t.Fatal("an old-cycle report refilled or cleared the persisted reset quota")
			}
			cfg.NextResetAt = manager.Snapshot().NextResetAt
			cfg.LastResetAt, cfg.CurrentUsed = tc.boundary.Unix(), cfg.Limit
			if action, err := restored.Configure(cfg); err != nil || action != ActionSuspend || restored.CanRun() {
				t.Fatalf("same-cycle panel exhaustion ignored after a delayed reset: action=%v error=%v", action, err)
			}
		})
	}
}
