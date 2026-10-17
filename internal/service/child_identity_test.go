package service

import (
	"context"
	"encoding/json"
	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/monitor"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"sync"
	"testing"
)

func childTestUsers(t *testing.T) []model.UserSpec {
	t.Helper()
	if strconv.IntSize != 64 {
		t.Skip("child identities require 64-bit ints")
	}
	base := int64(1) << 40
	return []model.UserSpec{{ID: 7, UUID: "parent"}, {ID: int(base + 1), UUID: "one", BillingUserID: 7, BillingNodeID: 11}, {ID: int(base + 2), UUID: "two", BillingUserID: 7, BillingNodeID: 12}}
}

func TestChildIdentityHashIncludesBillingAndIgnoresOrder(t *testing.T) {
	users := childTestUsers(t)
	before := computeUserHash(users)
	reordered := []model.UserSpec{users[2], users[0], users[1]}
	if computeUserHash(reordered) != before {
		t.Fatal("hash depends on order")
	}
	for _, field := range []string{"user", "node"} {
		changed := append([]model.UserSpec(nil), users...)
		if field == "user" {
			changed[1].BillingUserID++
		} else {
			changed[1].BillingNodeID++
		}
		if computeUserHash(changed) == before {
			t.Fatalf("billing %s excluded from hash", field)
		}
	}
}

type childRemovalKernel struct {
	fakeDebugKernel
	removed []model.UserSpec
}

func (k *childRemovalKernel) RemoveUsers(users []model.UserSpec) (int, error) {
	k.removed = append([]model.UserSpec(nil), users...)
	return len(users), nil
}

func TestChildIdentityAccountRemovalExpandsAliases(t *testing.T) {
	users := childTestUsers(t)
	s := reportService(controlplane.NewLocalControlPlane(&config.Config{}))
	k := &childRemovalKernel{fakeDebugKernel: fakeDebugKernel{running: true}}
	s.kernel = k
	s.updateUserState(users)
	s.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: users[1].ID}})
	if len(k.removed) != 1 || len(s.lastUsers) != 2 {
		t.Fatal("auth removal expanded to account")
	}
	s.updateUserState(users)
	s.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: 7}})
	if len(k.removed) != 3 || len(s.lastUsers) != 0 {
		t.Fatalf("account removal left aliases: %+v %+v", k.removed, s.lastUsers)
	}
	if len(expandAccountUsers(users[1:], []model.UserSpec{{ID: 7}})) != 3 {
		t.Fatal("account removal requires a parent auth identity")
	}
}

func TestChildIdentityReportAndSpoolRetainAuthIDs(t *testing.T) {
	users := childTestUsers(t)
	type batch struct {
		ReportID string           `json:"report_id"`
		Traffic  map[int][2]int64 `json:"traffic"`
		Alive    map[int][]string `json:"alive"`
		Online   map[int]int      `json:"online"`
	}
	var mu sync.Mutex
	var batches []batch
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/server/report" {
			w.Write([]byte(`{"data":true}`))
			return
		}
		var b batch
		if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		batches = append(batches, b)
		first := len(batches) == 1
		mu.Unlock()
		if first {
			w.Write([]byte(`{"data":false}`))
		} else {
			w.Write([]byte(`{"data":true}`))
		}
	}))
	defer server.Close()
	dir := t.TempDir()
	s := durableService(t, dir, server.URL)
	s.updateUserState(users)
	entries := []access.Entry{}
	traffic := make(map[int][2]int64)
	for i, u := range users {
		n := int64((i + 1) * 10)
		traffic[u.ID] = [2]int64{n, n + 1}
		entries = append(entries, destinationEntry(u.ID, n, n+1))
	}
	s.kernel = &accessTestKernel{fakeDebugKernel: fakeDebugKernel{running: true}, snapshot: access.Snapshot{Entries: entries}}
	s.configureAccess(&model.NodeSpec{AccessLog: &access.Config{Enabled: true}})
	s.tracker.Process(traffic, map[int]map[string]bool{7: {"192.0.2.1": true, "192.0.2.2": true}}, 3)
	metrics := s.buildMetrics(monitor.Status{})
	if metrics["active_users"] != 1 || metrics["total_users"] != 1 {
		t.Fatalf("inflated users: %+v", metrics)
	}
	if err := s.sendReport(context.Background()); err == nil {
		t.Fatal("expected unacknowledged batch")
	}
	original := s.pendingReport
	if original.Access == nil || len(original.Access.Entries) != 3 {
		t.Fatal("access auth identities lost")
	}
	restored := durableService(t, dir, server.URL)
	if err := restored.loadReportSpool(); err != nil {
		t.Fatal(err)
	}
	if got := restored.pendingReport; got.ReportID != original.ReportID || !reflect.DeepEqual(got.Traffic, traffic) || !reflect.DeepEqual(got.Access, original.Access) {
		t.Fatalf("spool changed auth identities: %+v", got)
	}
	if err := restored.sendReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 2 || batches[0].ReportID != batches[1].ReportID || !reflect.DeepEqual(batches[0].Traffic, batches[1].Traffic) || len(batches[0].Traffic) != 3 {
		t.Fatalf("retry changed auth traffic: %+v", batches)
	}
	if len(batches[0].Alive) != 1 || len(batches[0].Online) != 1 || batches[0].Online[7] != 2 {
		t.Fatalf("account online state: %+v", batches[0])
	}
}
