package limiter

import (
	"github.com/micah123321/mi-node/internal/model"
	"strconv"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestChildIdentitiesShareAccountLimitsAcrossUpdates(t *testing.T) {
	if strconv.IntSize != 64 {
		t.Skip("child identities require 64-bit ints")
	}
	base := int64(1) << 40
	users := []model.UserSpec{{ID: 7, UUID: "parent", SpeedLimit: 8, DeviceLimit: 2}, {ID: int(base + 1), UUID: "one", SpeedLimit: 8, DeviceLimit: 2, BillingUserID: 7}, {ID: int(base + 2), UUID: "two", SpeedLimit: 8, DeviceLimit: 2, BillingUserID: 7}}
	l := New()
	st := NewSpeedTracker(l)
	update := func() { l.UpdateUsers(users); st.UpdateBuckets() }
	update()
	shared := st.GetLimiter("parent")
	for _, u := range users {
		if shared == nil || st.GetLimiter(u.UUID) != shared {
			t.Fatalf("separate bucket: %s", u.UUID)
		}
		if limit, ok := l.GetDeviceLimitByUUID(u.UUID); !ok || limit != 2 {
			t.Fatalf("device limit: %d %v", limit, ok)
		}
	}
	now := time.Now()
	if !shared.AllowN(now, shared.Burst()) || st.GetLimiter("two").AllowN(now, 1) {
		t.Fatal("aliases did not consume the same tokens")
	}
	users = users[1:]
	users[0].UUID = "one-rotated"
	users[0].SpeedLimit, users[1].SpeedLimit = 4, 4
	users[0].DeviceLimit = 1
	update()
	if st.GetLimiter("one") != nil || st.GetLimiter("one-rotated") != shared || st.GetLimiter("two") != shared || shared.Limit() != 500000 || st.LimitedUserCount() != 1 {
		t.Fatal("account bucket lost during identity removal/rotation")
	}
	if limit, _ := l.GetDeviceLimitByUUID("two"); limit != 1 {
		t.Fatal("alias bypassed the shared device limit")
	}
	users[0].BillingUserID = 9
	update()
	if st.GetLimiter("one-rotated") == shared || st.GetLimiter("two") != shared {
		t.Fatal("account reassignment mixed token buckets")
	}
	users = nil
	update()
	if st.GetLimiter("two") != nil || st.HasLimits() || st.LimitedUserCount() != 0 {
		t.Fatal("deleted identities retained admission")
	}
}

func TestSpeedTrackerRestoresHeldConnectionBuckets(t *testing.T) {
	for _, withChild := range []bool{false, true} {
		name := "parent"
		if withChild {
			name = "parent_and_child"
		}
		t.Run(name, func(t *testing.T) {
			users := []model.UserSpec{{ID: 7, UUID: "parent", SpeedLimit: 8}}
			if withChild {
				users = append(users, model.UserSpec{ID: 8, UUID: "child", SpeedLimit: 8, BillingUserID: 7})
			}
			l := New()
			st := NewSpeedTracker(l)
			update := func(speed int) {
				for i := range users {
					users[i].SpeedLimit = speed
				}
				l.UpdateUsers(users)
				st.UpdateBuckets()
			}
			update(8)
			connections := make([]*rate.Limiter, len(users))
			for i, u := range users {
				connections[i] = st.GetLimiter(u.UUID)
				if connections[i] == nil || connections[i] != connections[0] {
					t.Fatalf("connection %s did not receive the account bucket", u.UUID)
				}
			}
			for _, speed := range []int{0, 8} {
				update(speed)
				want := rate.Limit(1_000_000)
				if speed == 0 {
					want = rate.Inf
				}
				for i, conn := range connections {
					if conn.Limit() != want || st.GetLimiter(users[i].UUID) != conn {
						t.Errorf("connection %s at %d Mbps: limit=%v, bucket identity preserved=%v", users[i].UUID, speed, conn.Limit(), st.GetLimiter(users[i].UUID) == conn)
					}
					if conn.AllowN(time.Now(), conn.Burst()+1) != (speed == 0) {
						t.Errorf("connection %s did not enforce %d Mbps", users[i].UUID, speed)
					}
				}
				wantCount := 1
				if speed == 0 {
					wantCount = 0
				}
				if st.HasLimits() != (speed > 0) || st.LimitedUserCount() != wantCount {
					t.Errorf("at %d Mbps: hasLimits=%v, limited accounts=%d", speed, st.HasLimits(), st.LimitedUserCount())
				}
			}
			now := time.Now()
			if connections[0].Burst() != 1_000_000 || !connections[0].AllowN(now, connections[0].Burst()) || connections[len(connections)-1].AllowN(now, 1) {
				t.Fatal("held connections did not share the restored token budget")
			}
		})
	}
}

func TestSpeedTrackerRetainedBucketCleanup(t *testing.T) {
	users := []model.UserSpec{
		{ID: 7, UUID: "parent", SpeedLimit: 8},
		{ID: 8, UUID: "one", SpeedLimit: 0, BillingUserID: 7},
		{ID: 9, UUID: "two", SpeedLimit: 16, BillingUserID: 7},
	}
	l := New()
	st := NewSpeedTracker(l)
	update := func() { l.UpdateUsers(users); st.UpdateBuckets() }
	update()
	shared := st.GetLimiter("parent")
	if shared == nil || shared.Limit() != 1_000_000 {
		t.Fatal("account did not use the strictest positive limit")
	}
	for i := range users {
		users[i].SpeedLimit = 0
	}
	update()
	users = users[1:]
	update()
	if st.GetLimiter("parent") != nil || st.GetLimiter("one") != shared || st.GetLimiter("two") != shared || shared.Limit() != rate.Inf {
		t.Fatal("removing an alias discarded the active unlimited account bucket")
	}
	users[0].SpeedLimit, users[1].SpeedLimit = 8, 16
	update()
	if st.GetLimiter("one") != shared || st.GetLimiter("two") != shared || shared.Limit() != 1_000_000 {
		t.Fatal("remaining aliases did not restore the strictest positive limit")
	}
	users[0].SpeedLimit, users[1].SpeedLimit = 0, 4
	update()
	if shared.Limit() != 500_000 {
		t.Fatal("an unlimited alias overrode the positive account limit")
	}
	for _, speed := range []int{0, 8} {
		users = []model.UserSpec{{ID: 8, UUID: "one", SpeedLimit: speed, BillingUserID: 7}}
		update()
		if st.GetLimiter("one") == nil {
			t.Fatal("active account lost its retained bucket")
		}
		users = nil
		update()
		if st.GetLimiter("one") != nil || st.HasLimits() || st.LimitedUserCount() != 0 || len(st.buckets) != 0 {
			t.Fatal("removed account retained its bucket")
		}
	}
}
