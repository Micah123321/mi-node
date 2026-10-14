// Package access collects destination metadata independently of billing counters.
package access

import (
	"net/netip"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/idna"
)

const (
	DefaultBucketSeconds = 300
	DefaultMaxEntries    = 2000
	MaxEntries           = 10000
)

type Config struct {
	Enabled       bool `json:"enabled"`
	BucketSeconds int  `json:"bucket_seconds"`
	MaxEntries    int  `json:"max_entries"`
}

type Entry struct {
	UserID       int    `json:"user_id"`
	Domain       string `json:"domain"`
	IP           string `json:"ip"`
	Port         int    `json:"port"`
	Network      string `json:"network"`
	DomainSource string `json:"domain_source"`
	BucketAt     int64  `json:"bucket_at"`
	FirstSeen    int64  `json:"first_seen"`
	LastSeen     int64  `json:"last_seen"`
	Upload       int64  `json:"u"`
	Download     int64  `json:"d"`
	Connections  int64  `json:"connections"`
}

type Snapshot struct {
	Entries        []Entry `json:"entries"`
	DroppedEntries int64   `json:"dropped_entries"`
}

// Target contains only observed request/sniffer metadata, never client addresses.
type Target struct {
	Domain       string
	IP           string
	Port         int
	Network      string
	DomainSource string
}

func (cfg Config) Normalize() Config {
	if cfg.BucketSeconds <= 0 || cfg.BucketSeconds > 86400 {
		cfg.BucketSeconds = DefaultBucketSeconds
	}
	if cfg.MaxEntries <= 0 {
		cfg.MaxEntries = DefaultMaxEntries
	}
	if cfg.MaxEntries > MaxEntries {
		cfg.MaxEntries = MaxEntries
	}
	return cfg
}

func (t Target) Normalize() Target {
	t.Network = strings.ToLower(t.Network)
	if t.Network != "tcp" && t.Network != "udp" {
		t.Network = "unknown"
	}
	t.Domain = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(t.Domain)), ".")
	if t.Domain != "" {
		d, err := idna.Lookup.ToASCII(t.Domain)
		if err != nil || len(d) > 253 || strings.ContainsAny(d, "/:@ ") {
			t.Domain = ""
		} else {
			t.Domain = d
		}
	}
	if t.IP != "" {
		if ip, err := netip.ParseAddr(t.IP); err == nil {
			t.IP = ip.Unmap().String()
		} else {
			t.IP = ""
		}
	}
	if t.Port < 0 || t.Port > 65535 {
		t.Port = 0
	}
	if t.Domain != "" {
		if t.DomainSource != "request" && t.DomainSource != "sniff" {
			t.DomainSource = "unknown"
		}
	} else if t.IP != "" {
		t.DomainSource = "ip"
	} else {
		t.DomainSource = "unknown"
		t.Port = 0
	}
	return t
}

type key struct {
	UserID int
	Target
	BucketAt int64
}

// Collector keeps at most MaxEntries rows, including unknown destinations.
// Rejected observations increment DroppedEntries; the billing reconciler fills
// their missing bytes. There is deliberately no unbounded overflow/user map.
type Collector struct {
	mu      sync.Mutex
	config  Config
	enabled atomic.Bool
	entries map[key]Entry
	dropped int64
	now     func() time.Time
}

func NewCollector(cfg Config) *Collector {
	c := &Collector{entries: make(map[key]Entry), now: time.Now}
	c.Configure(cfg)
	return c
}

// Configure retains pending bytes on disable. A smaller limit applies to new
// rows; existing rows remain bounded by the previous limit until the next drain.
func (c *Collector) Configure(cfg Config) {
	cfg = cfg.Normalize()
	c.mu.Lock()
	c.config = cfg
	c.enabled.Store(cfg.Enabled)
	c.mu.Unlock()
}

func (c *Collector) Enabled() bool { return c != nil && c.enabled.Load() }

func (c *Collector) record(uid int, target Target, u, d, connections int64) bool {
	if !c.Enabled() || uid <= 0 || (u <= 0 && d <= 0) {
		return false
	}
	if u < 0 {
		u = 0
	}
	if d < 0 {
		d = 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.config.Enabled {
		return false
	}
	at := c.now().Unix()
	bucket := at - at%int64(c.config.BucketSeconds)
	k := key{UserID: uid, Target: target, BucketAt: bucket}
	e, found := c.entries[k]
	if !found {
		if len(c.entries) >= c.config.MaxEntries {
			c.dropped++
			return false
		}
		e = Entry{UserID: uid, Domain: target.Domain, IP: target.IP, Port: target.Port, Network: target.Network, DomainSource: target.DomainSource, BucketAt: bucket, FirstSeen: at}
	}
	e.LastSeen = at
	e.Upload += u
	e.Download += d
	e.Connections += connections
	c.entries[k] = e
	return true
}

// Drain atomically takes current slices, including bytes from live connections.
// It also works while disabled; connection closure needs no deferred flush.
func (c *Collector) Drain() Snapshot {
	if c == nil {
		return Snapshot{Entries: []Entry{}}
	}
	c.mu.Lock()
	entries := c.entries
	dropped := c.dropped
	c.entries = make(map[key]Entry)
	c.dropped = 0
	c.mu.Unlock()
	out := Snapshot{Entries: make([]Entry, 0, len(entries)), DroppedEntries: dropped}
	for _, e := range entries {
		out.Entries = append(out.Entries, e)
	}
	sort.Slice(out.Entries, func(i, j int) bool {
		a, b := out.Entries[i], out.Entries[j]
		if a.UserID != b.UserID {
			return a.UserID < b.UserID
		}
		if a.BucketAt != b.BucketAt {
			return a.BucketAt < b.BucketAt
		}
		if a.Domain != b.Domain {
			return a.Domain < b.Domain
		}
		if a.IP != b.IP {
			return a.IP < b.IP
		}
		if a.Port != b.Port {
			return a.Port < b.Port
		}
		if a.Network != b.Network {
			return a.Network < b.Network
		}
		return a.DomainSource < b.DomainSource
	})
	return out
}

// Flow holds constant space per admitted stream/session. Connections counts
// admission once, on its first retained byte observation, not every packet or
// report slice. UDP rows reached later in the same session may have zero starts.
type Flow struct {
	mu        sync.Mutex
	collector *Collector
	userID    int
	target    Target
	started   bool
}

func (c *Collector) NewFlow(uid int, target Target) *Flow {
	return &Flow{collector: c, userID: uid, target: target.Normalize()}
}

func (f *Flow) SetTarget(target Target) {
	if f == nil {
		return
	}
	f.mu.Lock()
	f.target = target.Normalize()
	f.mu.Unlock()
}

func (f *Flow) Add(u, d int64) {
	if f == nil || !f.collector.Enabled() {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.add(f.target, u, d)
}

func (f *Flow) AddTo(target Target, u, d int64) {
	if f == nil || !f.collector.Enabled() {
		return
	}
	target = target.Normalize()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.add(target, u, d)
}

func (f *Flow) add(target Target, u, d int64) {
	var starts int64
	if !f.started {
		starts = 1
	}
	if f.collector.record(f.userID, target, u, d, starts) {
		f.started = true
	}
}
