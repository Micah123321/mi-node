package xray

import (
	"context"
	"fmt"
	"testing"

	"github.com/micah123321/mi-node/internal/model"
)

func TestHTTPStatsCounterIdentitiesPreserveCumulativeTraffic(t *testing.T) {
	users := []model.UserSpec{{ID: 1, UUID: "http-account-one"}, {ID: 2, UUID: "http-account-two"}}
	x, mgr := newStatsBackedXray(t, users, nil)
	x.protocol = "http"
	up := mustRegisterCounter(t, mgr, fmt.Sprintf("user>>>%s>>>traffic>>>uplink", users[0].UUID))
	down := mustRegisterCounter(t, mgr, fmt.Sprintf("user>>>%s>>>traffic>>>downlink", users[0].UUID))
	other := mustRegisterCounter(t, mgr, fmt.Sprintf("user>>>%s>>>traffic>>>downlink", users[1].UUID))
	canonical := mustRegisterCounter(t, mgr, counterName(1, "uplink"))
	up.Add(17)
	down.Add(31)
	other.Add(53)
	canonical.Add(7)

	check := func(wantOne, wantTwo [2]int64) {
		t.Helper()
		traffic, _, _, err := x.GetUserTraffic(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if traffic[1] != wantOne || traffic[2] != wantTwo {
			t.Fatalf("traffic=%v, want %v and %v", traffic, wantOne, wantTwo)
		}
	}
	check([2]int64{17, 31}, [2]int64{0, 53})
	check([2]int64{17, 31}, [2]int64{0, 53})

	// Previously admitted connections may still write after their user disappears.
	x.users = users[1:]
	up.Add(11)
	down.Add(13)
	check([2]int64{28, 44}, [2]int64{0, 53})

	// Credential rotation must keep the old connection's counter identity alive.
	x.users = []model.UserSpec{{ID: 1, UUID: "http-account-one-new"}, users[1]}
	rotated := mustRegisterCounter(t, mgr, "user>>>http-account-one-new>>>traffic>>>uplink")
	rotated.Add(19)
	up.Add(23)
	check([2]int64{70, 44}, [2]int64{0, 53})
	if canonical.Value() != 7 {
		t.Fatal("HTTP consumed a counter outside its native username identity")
	}
}

func TestHTTPStatsIdenticalCounterIdentityIsNotDoubleCounted(t *testing.T) {
	users := []model.UserSpec{{ID: 1, UUID: userEmail(1)}}
	x, mgr := newStatsBackedXray(t, users, nil)
	x.protocol = "http"
	up := mustRegisterCounter(t, mgr, counterName(1, "uplink"))
	up.Add(17)
	for i := 0; i < 2; i++ {
		traffic, _, _, err := x.GetUserTraffic(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if traffic[1] != [2]int64{17, 0} {
			t.Fatalf("counter identity counted more than once: %v", traffic)
		}
	}
}

func TestHTTPStatsRetainsIdentityBeforeAddUsersMetadataChange(t *testing.T) {
	users := []model.UserSpec{{ID: 1, UUID: "http-old"}}
	x, mgr := newStatsBackedXray(t, users, nil)
	x.protocol = "http"
	x.nodeConfig = &model.NodeSpec{Protocol: "http"}
	up := mustRegisterCounter(t, mgr, "user>>>http-old>>>traffic>>>uplink")
	up.Add(37)
	if added, err := x.AddUsers([]model.UserSpec{{ID: 1, UUID: "http-new"}}); err != nil || added != 0 {
		t.Fatalf("metadata update: %d %v", added, err)
	}
	up.Add(17)
	traffic, _, _, err := x.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if traffic[1] != [2]int64{54, 0} {
		t.Fatalf("first update lost old identity: %v", traffic)
	}
}

func TestHTTPStatsUsernameMatchingAnotherUserEmailKeepsOwner(t *testing.T) {
	users := []model.UserSpec{{ID: 1, UUID: userEmail(2)}, {ID: 2, UUID: "http-two"}}
	x, mgr := newStatsBackedXray(t, users, nil)
	x.protocol = "http"
	one := mustRegisterCounter(t, mgr, counterName(2, "uplink"))
	two := mustRegisterCounter(t, mgr, "user>>>http-two>>>traffic>>>uplink")
	one.Add(17)
	two.Add(31)
	for i := 0; i < 2; i++ {
		traffic, _, _, err := x.GetUserTraffic(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if traffic[1] != [2]int64{17, 0} || traffic[2] != [2]int64{31, 0} {
			t.Fatalf("HTTP username assigned to wrong user: %v", traffic)
		}
	}
}

func TestStatsDoesNotConsumeUnrelatedUUIDCounter(t *testing.T) {
	users := []model.UserSpec{{ID: 1, UUID: "vless-account"}}
	x, mgr := newStatsBackedXray(t, users, nil)
	x.protocol = "vless"
	canonical := mustRegisterCounter(t, mgr, counterName(1, "uplink"))
	unrelated := mustRegisterCounter(t, mgr, "user>>>vless-account>>>traffic>>>uplink")
	canonical.Add(7)
	unrelated.Add(11)
	traffic, _, _, err := x.GetUserTraffic(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if traffic[1] != [2]int64{7, 0} || unrelated.Value() != 11 {
		t.Fatalf("unrelated counter consumed: traffic=%v value=%d", traffic, unrelated.Value())
	}
}
