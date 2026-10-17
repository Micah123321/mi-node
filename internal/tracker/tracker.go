package tracker

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"sort"
	"sync"
	"sync/atomic"

	"github.com/micah123321/mi-node/internal/nlog"
)

// snapshot is an immutable point-in-time view of tracker state.
// It is swapped atomically so readers never block writers.
type snapshot struct {
	aliveIPs  map[int]map[string]bool // userID → set of source IPs
	online    map[int]int             // userID → distinct IP count
	connCount int
	inSpeed   int64
	outSpeed  int64
}

// Tracker computes per-user traffic deltas from cumulative counters
// provided by the kernel, and accumulates totals for panel reporting.
//
// Architecture: the kernel maintains per-user atomic counters and IP sets.
// Each tick, the service calls Process() with cumulative per-user traffic.
// Tracker computes deltas against the previous cycle's values — O(users),
// not O(connections).
//
// Thread safety: Process() and traffic flushes share mu. Online/connection
// readers use an immutable snapshot; pending traffic stays in one owned map
// until FlushTraffic hands it to the report worker.
type Tracker struct {
	mu sync.Mutex

	// lastSeen stores the cumulative traffic from the previous Process() call.
	// Protected by mu — only written by Process.
	lastSeen map[int][2]int64 // userID → [upload, download] cumulative

	// pending accumulates deltas between flushes.
	// Protected by mu — written by Process, drained by FlushTraffic.
	pendingTraffic map[int][2]int64

	// live holds the current snapshot, swapped atomically.
	// Readers load this pointer without any lock.
	live atomic.Pointer[snapshot]

	// lastAliveIPsHash detects changes to avoid duplicate reports.
	lastAliveIPsHash string
}

func New() *Tracker {
	t := &Tracker{
		lastSeen:       make(map[int][2]int64),
		pendingTraffic: make(map[int][2]int64),
	}
	// Publish initial empty snapshot.
	t.live.Store(&snapshot{
		aliveIPs: make(map[int]map[string]bool),
		online:   make(map[int]int),
	})
	return t
}

// Process computes per-user traffic deltas from cumulative kernel counters.
// Also stores alive IPs and connection count. O(users).
//
// This is the only writer to lastSeen and pendingTraffic.
// After computing deltas, it publishes a new snapshot for lock-free reads.
func (t *Tracker) Process(
	cumTraffic map[int][2]int64,
	kernelAliveIPs map[int]map[string]bool,
	connCount int,
) (uploadDelta int64, downloadDelta int64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	var cycleIn, cycleOut int64
	previous := t.lastSeen
	missing := len(previous)
	// Rebuild after a large population drop so the map can release peak capacity.
	rebuild := len(cumTraffic) < len(previous)/2
	if rebuild {
		t.lastSeen = make(map[int][2]int64, len(cumTraffic))
	}

	for uid, cum := range cumTraffic {
		prev, seen := previous[uid]
		if seen {
			missing--
		}
		deltaUp := cum[0] - prev[0]
		deltaDown := cum[1] - prev[1]

		// Guard against counter reset (kernel restart).
		if deltaUp < 0 {
			deltaUp = cum[0]
		}
		if deltaDown < 0 {
			deltaDown = cum[1]
		}

		t.lastSeen[uid] = cum

		if deltaUp > 0 || deltaDown > 0 {
			cur := t.pendingTraffic[uid]
			cur[0] += deltaUp
			cur[1] += deltaDown
			t.pendingTraffic[uid] = cur

			cycleOut += deltaUp
			cycleIn += deltaDown
		}
	}

	// Successful kernel snapshots are complete; absent counters no longer have a
	// baseline. Pending bytes remain independent until acknowledged by reporting.
	if missing > 0 && !rebuild {
		for uid := range t.lastSeen {
			if _, present := cumTraffic[uid]; !present {
				delete(t.lastSeen, uid)
			}
		}
	}

	// Compute online from alive IPs.
	online := make(map[int]int, len(kernelAliveIPs))
	for uid, ips := range kernelAliveIPs {
		online[uid] = len(ips)
	}

	// Publish new snapshot (readers will see this atomically).
	t.live.Store(&snapshot{
		aliveIPs:  kernelAliveIPs, // kernel provides fresh copy each tick
		online:    online,
		connCount: connCount,
		inSpeed:   cycleIn,
		outSpeed:  cycleOut,
	})

	return cycleOut, cycleIn
}

// FlushTraffic returns accumulated per-user traffic and resets the pending buffer.
// The returned map belongs to the caller; later samples use a fresh buffer.
func (t *Tracker) FlushTraffic() map[int][2]int64 {
	t.mu.Lock()
	data := t.pendingTraffic
	t.pendingTraffic = make(map[int][2]int64, len(data))
	t.mu.Unlock()
	return data
}

// RestoreTraffic adds traffic back (used when push to panel fails).
func (t *Tracker) RestoreTraffic(data map[int][2]int64) {
	t.mu.Lock()
	for uid, d := range data {
		cur := t.pendingTraffic[uid]
		cur[0] += d[0]
		cur[1] += d[1]
		t.pendingTraffic[uid] = cur
	}
	t.mu.Unlock()
}

// HasTraffic returns true if there is accumulated traffic to report.
func (t *Tracker) HasTraffic() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.pendingTraffic) > 0
}

// FlushAliveIPs returns per-user alive IPs.
// Returns an owned snapshot, or nil if unchanged. Async reports may retain it.
func (t *Tracker) FlushAliveIPs() map[int][]string {
	s := t.live.Load()

	t.mu.Lock()
	defer t.mu.Unlock()

	// Calculate hash of current aliveIPs
	currentHash := calcAliveIPsHash(s.aliveIPs)

	// If no changes, return nil to avoid duplicate reporting
	if currentHash == t.lastAliveIPsHash {
		return nil
	}

	t.lastAliveIPsHash = currentHash

	data := make(map[int][]string, len(s.aliveIPs))
	for uid, ips := range s.aliveIPs {
		buf := make([]string, 0, len(ips))
		for ip := range ips {
			buf = append(buf, ip)
		}
		data[uid] = buf
	}
	return data
}

// calcAliveIPsHash computes a deterministic hash for change detection.
func calcAliveIPsHash(aliveIPs map[int]map[string]bool) string {
	if len(aliveIPs) == 0 {
		return ""
	}

	h := sha256.New()

	// Sort user IDs for consistent hashing
	userIDs := make([]int, 0, len(aliveIPs))
	for uid := range aliveIPs {
		userIDs = append(userIDs, uid)
	}
	sort.Ints(userIDs)

	for _, uid := range userIDs {
		ips := aliveIPs[uid]
		// Sort IPs for consistent hashing
		ipList := make([]string, 0, len(ips))
		for ip := range ips {
			ipList = append(ipList, ip)
		}
		sort.Strings(ipList)

		// Write user ID and IPs to hash
		var id [8]byte
		binary.LittleEndian.PutUint64(id[:], uint64(uid))
		h.Write(id[:])
		for _, ip := range ipList {
			h.Write([]byte(ip))
			h.Write([]byte{0})
		}
	}

	return hex.EncodeToString(h.Sum(nil))
}

// CurrentOnline returns a snapshot copy of user_id → device count (distinct IPs).
// Lock-free: reads from live snapshot.
func (t *Tracker) CurrentOnline() map[int]int {
	s := t.live.Load()
	cp := make(map[int]int, len(s.online))
	for k, v := range s.online {
		cp[k] = v
	}
	return cp
}

// LogStats logs current tracking statistics.
// Lock-free: reads from live snapshot.
func (t *Tracker) LogStats() {
	s := t.live.Load()
	nlog.TrackerStats(s.connCount, len(s.online))
}

// ActiveConnections returns the last observed active connection count.
// Lock-free: reads from live snapshot.
func (t *Tracker) ActiveConnections() int {
	return t.live.Load().connCount
}

// TotalConnections is deprecated — no longer tracked per-connection.
// Returns 0 for backward compatibility.
func (t *Tracker) TotalConnections() int64 {
	return 0
}

// InboundSpeed returns the last observed inbound (download) speed in bytes/second.
// Lock-free: reads from live snapshot.
func (t *Tracker) InboundSpeed() int64 {
	return t.live.Load().inSpeed / 10
}

// OutboundSpeed returns the last observed outbound (upload) speed in bytes/second.
// Lock-free: reads from live snapshot.
func (t *Tracker) OutboundSpeed() int64 {
	return t.live.Load().outSpeed / 10
}
