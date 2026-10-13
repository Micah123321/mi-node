package trafficlimit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	StatusNormal    = "normal"
	StatusSuspended = "suspended"
)

type Action int

const (
	ActionNone Action = iota
	ActionSuspend
	ActionResume
)

type Config struct {
	Enabled     bool
	Limit       int64
	ResetDay    int
	ResetTime   string
	Timezone    string
	CurrentUsed int64
	LastResetAt int64
	NextResetAt int64
	SuspendedAt int64
	Status      string
}

type State struct {
	Used        int64  `json:"used"`
	Suspended   bool   `json:"suspended"`
	LastResetAt int64  `json:"last_reset_at"`
	NextResetAt int64  `json:"next_reset_at"`
	SuspendedAt int64  `json:"suspended_at"`
	Timezone    string `json:"timezone"`
}

type Snapshot struct {
	Enabled     bool   `json:"enabled"`
	Limit       int64  `json:"limit"`
	Used        int64  `json:"used"`
	Suspended   bool   `json:"suspended"`
	LastResetAt int64  `json:"last_reset_at"`
	NextResetAt int64  `json:"next_reset_at"`
	SuspendedAt int64  `json:"suspended_at"`
	Status      string `json:"status"`
}

type Manager struct {
	mu    sync.Mutex
	path  string
	now   func() time.Time
	cfg   Config
	state State
}

func New(path string, now func() time.Time) *Manager {
	if now == nil {
		now = time.Now
	}
	m := &Manager{path: path, now: now}
	_ = m.load()
	return m
}

func (m *Manager) Configure(cfg Config) (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	wasSuspended := m.state.Suspended
	m.cfg = normalizeConfig(cfg)

	if !m.cfg.Enabled {
		m.state = State{}
		return actionForTransition(wasSuspended, false), m.save()
	}

	now := m.now()
	if m.cfg.LastResetAt > m.state.LastResetAt {
		m.state.Used = maxInt64(0, m.cfg.CurrentUsed)
		m.state.LastResetAt = m.cfg.LastResetAt
		m.state.Suspended = false
		m.state.SuspendedAt = 0
	} else if m.cfg.LastResetAt == m.state.LastResetAt && m.cfg.CurrentUsed > m.state.Used {
		// Reconcile same-cycle usage monotonically; older cycles must not refill a reset quota.
		m.state.Used = m.cfg.CurrentUsed
	}

	m.state.Timezone = m.cfg.Timezone
	if m.cfg.NextResetAt > m.state.LastResetAt {
		m.state.NextResetAt = m.cfg.NextResetAt
	} else {
		m.refreshNextResetLocked(now)
	}

	if m.state.NextResetAt > 0 && now.Unix() >= m.state.NextResetAt {
		m.resetLocked(now)
	}

	if m.state.Used >= m.cfg.Limit {
		m.state.Suspended = true
		if m.state.SuspendedAt == 0 {
			m.state.SuspendedAt = maxInt64(now.Unix(), m.cfg.SuspendedAt)
		}
	} else {
		m.state.Suspended = false
		m.state.SuspendedAt = 0
	}

	return actionForTransition(wasSuspended, m.state.Suspended), m.save()
}

func (m *Manager) AddTraffic(upload, download int64) (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.enabledLocked() {
		return ActionNone, nil
	}

	if action, err := m.resetIfDueLocked(m.now()); action != ActionNone || err != nil {
		return action, err
	}

	delta := maxInt64(0, upload) + maxInt64(0, download)
	if delta == 0 {
		return ActionNone, nil
	}

	m.state.Used += delta
	if !m.state.Suspended && m.state.Used >= m.cfg.Limit {
		m.state.Suspended = true
		m.state.SuspendedAt = m.now().Unix()
		return ActionSuspend, m.save()
	}

	return ActionNone, m.save()
}

func (m *Manager) CheckReset() (Action, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if !m.enabledLocked() {
		return ActionNone, nil
	}

	return m.resetIfDueLocked(m.now())
}

func (m *Manager) CanRun() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return !m.enabledLocked() || !m.state.Suspended
}

func (m *Manager) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	status := StatusNormal
	if m.state.Suspended {
		status = StatusSuspended
	}

	return Snapshot{
		Enabled:     m.enabledLocked(),
		Limit:       m.cfg.Limit,
		Used:        m.state.Used,
		Suspended:   m.state.Suspended,
		LastResetAt: m.state.LastResetAt,
		NextResetAt: m.state.NextResetAt,
		SuspendedAt: m.state.SuspendedAt,
		Status:      status,
	}
}

func (m *Manager) enabledLocked() bool {
	return m.cfg.Enabled && m.cfg.Limit > 0
}

func (m *Manager) resetIfDueLocked(now time.Time) (Action, error) {
	if m.state.NextResetAt <= 0 || now.Unix() < m.state.NextResetAt {
		return ActionNone, nil
	}
	wasSuspended := m.state.Suspended
	m.resetLocked(now)
	return actionForTransition(wasSuspended, false), m.save()
}

func (m *Manager) resetLocked(now time.Time) {
	m.state.Used = 0
	m.state.Suspended = false
	m.state.SuspendedAt = 0
	// Use the cycle boundary, rather than the later timer tick, as reset identity.
	boundary := scheduledResetAtOrBefore(now, m.cfg).Unix()
	if m.state.NextResetAt > boundary && m.state.NextResetAt <= now.Unix() {
		boundary = m.state.NextResetAt
	}
	m.state.LastResetAt = boundary
	m.state.Timezone = m.cfg.Timezone
	m.state.NextResetAt = nextResetAfter(now, m.cfg).Unix()
}

func (m *Manager) refreshNextResetLocked(now time.Time) {
	if m.cfg.NextResetAt > now.Unix() {
		m.state.NextResetAt = m.cfg.NextResetAt
		return
	}
	if m.state.NextResetAt <= 0 || m.state.NextResetAt <= now.Unix() {
		m.state.NextResetAt = nextResetAfter(now, m.cfg).Unix()
	}
}

func (m *Manager) load() error {
	if m.path == "" {
		return nil
	}
	data, err := os.ReadFile(m.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &m.state)
}

func (m *Manager) save() error {
	if m.path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m.state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(m.path, data, 0o644)
}

func normalizeConfig(cfg Config) Config {
	if cfg.Limit <= 0 {
		cfg.Enabled = false
	}
	cfg.ResetDay = clamp(cfg.ResetDay, 1, 31)
	if cfg.ResetDay == 0 {
		cfg.ResetDay = 1
	}
	cfg.ResetTime = normalizeResetTime(cfg.ResetTime)
	cfg.Timezone = normalizeTimezone(cfg.Timezone)
	cfg.CurrentUsed = maxInt64(0, cfg.CurrentUsed)
	return cfg
}

func nextResetAfter(from time.Time, cfg Config) time.Time {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := from.In(loc)
	hour, minute := parseResetTime(cfg.ResetTime)
	target := targetForMonth(local.Year(), local.Month(), cfg.ResetDay, hour, minute, loc)
	if target.After(local) {
		return target
	}
	next := local.AddDate(0, 1, 0)
	return targetForMonth(next.Year(), next.Month(), cfg.ResetDay, hour, minute, loc)
}

func scheduledResetAtOrBefore(now time.Time, cfg Config) time.Time {
	loc, err := time.LoadLocation(cfg.Timezone)
	if err != nil {
		loc = time.UTC
	}
	local := now.In(loc)
	hour, minute := parseResetTime(cfg.ResetTime)
	target := targetForMonth(local.Year(), local.Month(), cfg.ResetDay, hour, minute, loc)
	if target.After(local) {
		previous := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -1, 0)
		return targetForMonth(previous.Year(), previous.Month(), cfg.ResetDay, hour, minute, loc)
	}
	return target
}

func targetForMonth(year int, month time.Month, day, hour, minute int, loc *time.Location) time.Time {

	first := time.Date(year, month, 1, hour, minute, 0, 0, loc)
	lastDay := first.AddDate(0, 1, -1).Day()
	return time.Date(year, month, clamp(day, 1, lastDay), hour, minute, 0, 0, loc)
}

func normalizeResetTime(value string) string {
	hour, minute := parseResetTime(value)
	return fmt.Sprintf("%02d:%02d", hour, minute)
}

func parseResetTime(value string) (int, int) {
	parts := strings.Split(strings.TrimSpace(value), ":")
	if len(parts) != 2 {
		return 0, 0
	}
	hour, errHour := strconv.Atoi(parts[0])
	minute, errMinute := strconv.Atoi(parts[1])
	if errHour != nil || errMinute != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, 0
	}
	return hour, minute
}

func normalizeTimezone(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return "UTC"
	}
	if _, err := time.LoadLocation(value); err != nil {
		return "UTC"
	}
	return value
}

func actionForTransition(wasSuspended, suspended bool) Action {
	switch {
	case !wasSuspended && suspended:
		return ActionSuspend
	case wasSuspended && !suspended:
		return ActionResume
	default:
		return ActionNone
	}
}

func clamp(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
