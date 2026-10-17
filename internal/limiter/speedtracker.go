package limiter

import (
	"sync"
	"sync/atomic"

	"github.com/micah123321/mi-node/internal/model"
	"golang.org/x/time/rate"
)

type SpeedTrackerLogCallback func(msg string)

// SpeedTracker shares one token bucket across every auth identity of an account.
type SpeedTracker struct {
	limiter   *Limiter
	mu        sync.RWMutex
	buckets   map[int]*rate.Limiter
	uuidMap   map[string]int
	speeds    map[int]int
	hasLimits atomic.Bool
	logFunc   SpeedTrackerLogCallback
}

func NewSpeedTracker(l *Limiter) *SpeedTracker {
	return &SpeedTracker{limiter: l, buckets: make(map[int]*rate.Limiter), uuidMap: make(map[string]int), speeds: make(map[int]int)}
}

func (t *SpeedTracker) SetLogCallback(f SpeedTrackerLogCallback) { t.logFunc = f }

func (t *SpeedTracker) UpdateBuckets() {
	t.limiter.mu.RLock()
	users := make([]model.UserSpec, 0, len(t.limiter.users))
	for _, u := range t.limiter.users {
		users = append(users, u)
	}
	t.limiter.mu.RUnlock()
	accounts := model.AccountLimits(users)

	t.mu.Lock()
	t.uuidMap = make(map[string]int, len(users))
	t.speeds = make(map[int]int, len(accounts))
	for _, u := range users {
		t.uuidMap[u.UUID] = u.AccountID()
	}
	for id, u := range accounts {
		if u.SpeedLimit > 0 {
			t.speeds[id] = u.SpeedLimit
		}
	}
	for id, lim := range t.buckets {
		if speed := t.speeds[id]; speed > 0 {
			bps := speed * 1_000_000 / 8
			lim.SetLimit(rate.Limit(bps))
			lim.SetBurst(max(bps, 64*1024))
		} else if _, active := accounts[id]; active {
			// Existing connections keep this bucket and must see later limit changes.
			lim.SetLimit(rate.Inf)
		} else {
			delete(t.buckets, id)
		}
	}
	t.hasLimits.Store(len(t.speeds) > 0)
	t.mu.Unlock()
	if t.logFunc != nil {
		t.logFunc("buckets updated")
	}
}

func (t *SpeedTracker) GetLimiter(uuid string) *rate.Limiter {
	t.mu.RLock()
	id, exists := t.uuidMap[uuid]
	lim := t.buckets[id]
	t.mu.RUnlock()
	if !exists {
		return nil
	}
	if lim != nil {
		return lim
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	// Refresh the mapping after the lock upgrade; a user update may have raced us.
	id, exists = t.uuidMap[uuid]
	if !exists || t.speeds[id] <= 0 {
		return nil
	}
	if lim = t.buckets[id]; lim == nil {
		bps := t.speeds[id] * 1_000_000 / 8
		lim = rate.NewLimiter(rate.Limit(bps), max(bps, 64*1024))
		t.buckets[id] = lim
	}
	return lim
}

func (t *SpeedTracker) HasLimits() bool { return t.hasLimits.Load() }

func (t *SpeedTracker) LimitedUserCount() int {
	t.mu.RLock()
	defer t.mu.RUnlock()
	count := 0
	for id := range t.buckets {
		if t.speeds[id] > 0 {
			count++
		}
	}
	return count
}
