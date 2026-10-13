package service

import (
	"context"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/trafficlimit"
)

type quotaReloadKernel struct {
	bandwidthKernel
}

func (k *quotaReloadKernel) Protocols() []string { return []string{"hysteria"} }

func quotaReloadService(t *testing.T) (*Service, *quotaReloadKernel, time.Time) {
	t.Helper()
	svc, existing := bandwidthService(t)
	k := &quotaReloadKernel{bandwidthKernel: *existing}
	k.running = true
	svc.kernel = k
	now := time.Now()
	svc.trafficLimit = trafficlimit.New("", func() time.Time { return now })
	svc.lastConfig = &model.NodeSpec{
		Protocol: "hysteria", Version: 2, ServerPort: 42618, ServerName: "node.example.invalid",
		TrafficLimit: &model.TrafficLimitSpec{
			Enabled: true, Limit: 1000, CurrentUsed: 10, Timezone: "UTC",
			LastResetAt: now.Add(-time.Hour).Unix(), NextResetAt: now.Add(time.Hour).Unix(),
		},
	}
	if err := svc.ensureTLSCertificate(context.Background(), svc.lastConfig); err != nil {
		t.Fatal(err)
	}
	svc.configureTrafficLimit(svc.lastConfig)
	svc.lastConfigHash = computeConfigHash(svc.lastConfig)
	svc.lastUserHash = computeUserHash(svc.lastUsers)
	return svc, k, now
}

func quotaConfigCopy(nc *model.NodeSpec) *model.NodeSpec {
	copy := *nc
	quota := *nc.TrafficLimit
	copy.TrafficLimit = &quota
	return &copy
}

func deliverQuotaConfig(svc *Service, transport string, nc *model.NodeSpec) {
	ctx := context.Background()
	if transport == "REST" {
		svc.applyPullResult(ctx, pullResult{config: nc, configHash: computeConfigHash(nc),
			users: svc.lastUsers, userHash: svc.lastUserHash})
	} else {
		svc.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncConfig, Config: nc})
	}
}

func TestQuotaUpdatesDoNotReloadProtocol(t *testing.T) {
	for _, transport := range []string{"REST", "WS"} {
		t.Run(transport, func(t *testing.T) {
			svc, k, _ := quotaReloadService(t)
			originalHash := svc.lastConfigHash
			for i := 0; i < 3; i++ {
				if action, err := svc.trafficLimit.AddTraffic(1, 2); err != nil || action != trafficlimit.ActionNone {
					t.Fatalf("AddTraffic: action=%v error=%v", action, err)
				}
				updated := quotaConfigCopy(svc.lastConfig)
				updated.TrafficLimit.CurrentUsed = svc.trafficLimit.Snapshot().Used
				updated.TrafficLimit.NextResetAt += 60
				deliverQuotaConfig(svc, transport, updated)
				if k.reloadCalls != 0 || k.startCalls != 0 || !k.IsRunning() {
					t.Fatalf("quota-only update interrupted the kernel: reloads=%d starts=%d running=%v", k.reloadCalls, k.startCalls, k.IsRunning())
				}
				if svc.lastConfig != updated || svc.lastConfigHash != originalHash {
					t.Fatal("quota update was discarded or changed the protocol hash")
				}
				if snapshot := svc.trafficLimit.Snapshot(); snapshot.NextResetAt != updated.TrafficLimit.NextResetAt {
					t.Fatal("quota manager did not receive the updated schedule")
				}
			}
		})
	}
}

func TestQuotaUpdatesStillSuspendAndResumeProtocol(t *testing.T) {
	for _, transport := range []string{"REST", "WS"} {
		t.Run(transport, func(t *testing.T) {
			svc, k, now := quotaReloadService(t)
			updated := quotaConfigCopy(svc.lastConfig)
			updated.TrafficLimit.Limit = 10
			deliverQuotaConfig(svc, transport, updated)
			if k.IsRunning() || !svc.runBlocked() || k.reloadCalls != 0 {
				t.Fatal("lowering quota to current usage must stop the kernel without reloading")
			}
			reset := quotaConfigCopy(updated)
			reset.TrafficLimit.CurrentUsed = 0
			reset.TrafficLimit.LastResetAt = now.Unix()
			deliverQuotaConfig(svc, transport, reset)
			if !k.IsRunning() || svc.runBlocked() || k.startCalls != 1 || k.reloadCalls != 0 {
				t.Fatalf("quota reset did not resume exactly once: starts=%d reloads=%d running=%v", k.startCalls, k.reloadCalls, k.IsRunning())
			}
			if snapshot := svc.trafficLimit.Snapshot(); snapshot.Used != 0 || snapshot.Suspended {
				t.Fatalf("quota reset state is incorrect: %+v", snapshot)
			}
		})
	}
}

func TestProtocolConfigChangesStillReload(t *testing.T) {
	for _, transport := range []string{"REST", "WS"} {
		t.Run(transport, func(t *testing.T) {
			svc, k, _ := quotaReloadService(t)
			updated := quotaConfigCopy(svc.lastConfig)
			updated.ServerPort++
			deliverQuotaConfig(svc, transport, updated)
			if k.reloadCalls != 1 || svc.lastConfig.ServerPort != updated.ServerPort {
				t.Fatal("listening port change must reload the kernel")
			}
		})
	}
}

type connectedQuotaPush struct{}

func (connectedQuotaPush) Run(context.Context)               {}
func (connectedQuotaPush) IsConnected() bool                 { return true }
func (connectedQuotaPush) SendDeviceReport(map[int][]string) {}

func TestCertificateRenewalStillReloadsProtocol(t *testing.T) {
	svc, k, _ := quotaReloadService(t)
	svc.wsClient = connectedQuotaPush{}
	svc.applyCertificateRenewal(context.Background(), true)
	svc.applyCertificateRenewal(context.Background(), false)
	if k.reloadCalls != 1 {
		t.Fatal("certificate renewal must reload exactly once while WS is connected")
	}
}

type quotaPollSource struct {
	controlplane.Source
	snapshot controlplane.Snapshot
}

func (s quotaPollSource) SupportsPolling() bool { return true }
func (s quotaPollSource) Poll(context.Context) (controlplane.Snapshot, error) {
	return s.snapshot, nil
}

func TestWSFailureReconcilesQuotaWithoutReload(t *testing.T) {
	svc, k, _ := quotaReloadService(t)
	updated := quotaConfigCopy(svc.lastConfig)
	updated.TrafficLimit.CurrentUsed++
	updated.TrafficLimit.NextResetAt += 60
	svc.source = quotaPollSource{snapshot: controlplane.Snapshot{Config: updated, Users: svc.lastUsers}}
	svc.pullResults = make(chan pullResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.handleWSStatus(ctx, controlplane.StatusChange{Connected: false})
	select {
	case result := <-svc.pullResults:
		if result.config != updated {
			t.Fatal("REST reconciliation discarded the quota-only config")
		}
		svc.applyPullResult(ctx, result)
	case <-time.After(5 * time.Second):
		t.Fatal("WS failure did not schedule REST reconciliation")
	}
	if k.reloadCalls != 0 || !k.IsRunning() || svc.lastConfig != updated {
		t.Fatal("WS failure and quota growth must preserve the running protocol")
	}
	if svc.trafficLimit.Snapshot().NextResetAt != updated.TrafficLimit.NextResetAt {
		t.Fatal("REST reconciliation did not update quota state")
	}
}

func TestQuotaReportedUsageStillSuspendsProtocol(t *testing.T) {
	for _, transport := range []string{"REST", "WS"} {
		t.Run(transport, func(t *testing.T) {
			svc, k, _ := quotaReloadService(t)
			updated := quotaConfigCopy(svc.lastConfig)
			updated.TrafficLimit.CurrentUsed = updated.TrafficLimit.Limit
			deliverQuotaConfig(svc, transport, updated)
			if k.IsRunning() || !svc.runBlocked() || k.reloadCalls != 0 {
				t.Fatal("authoritative quota exhaustion must stop the kernel without a reload")
			}
		})
	}
}

func TestQuotaUpdateStartsProtocolWhenFirstUsersArrive(t *testing.T) {
	svc, k, _ := quotaReloadService(t)
	k.running = false
	svc.lastUsers = nil
	svc.lastUserHash = computeUserHash(nil)
	updated := quotaConfigCopy(svc.lastConfig)
	updated.TrafficLimit.CurrentUsed++
	users := []model.UserSpec{{ID: 2, UUID: "first-fixture-user"}}
	svc.applyPullResult(context.Background(), pullResult{config: updated, configHash: computeConfigHash(updated),
		users: users, userHash: computeUserHash(users)})
	if !k.IsRunning() || k.startCalls != 1 || k.reloadCalls != 0 || len(svc.lastUsers) != 1 {
		t.Fatal("first user list must start the cached protocol without a configuration reload")
	}
}

type delayedQuotaSource struct {
	controlplane.Source
	snapshot       controlplane.Snapshot
	started        chan struct{}
	release        chan struct{}
	resyncRequests int
}

func (s *delayedQuotaSource) SupportsPolling() bool { return true }
func (s *delayedQuotaSource) RequestResync()        { s.resyncRequests++ }
func (s *delayedQuotaSource) Poll(ctx context.Context) (controlplane.Snapshot, error) {
	close(s.started)
	select {
	case <-s.release:
		return s.snapshot, nil
	case <-ctx.Done():
		return controlplane.Snapshot{}, ctx.Err()
	}
}

func TestStaleRESTSnapshotDoesNotRollbackWSPush(t *testing.T) {
	for _, update := range []string{"config", "users"} {
		t.Run(update, func(t *testing.T) {
			svc, k, _ := quotaReloadService(t)
			oldConfig := quotaConfigCopy(svc.lastConfig)
			oldConfig.TrafficLimit.CurrentUsed++
			source := &delayedQuotaSource{snapshot: controlplane.Snapshot{Config: oldConfig, Users: svc.lastUsers}, started: make(chan struct{}), release: make(chan struct{})}
			svc.source = source
			svc.pullResults = make(chan pullResult, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc.pullViaAPIAsync(ctx)
			select {
			case <-source.started:
			case <-time.After(5 * time.Second):
				t.Fatal("REST poll did not start")
			}
			latestConfig := svc.lastConfig
			latestUsers := svc.lastUsers
			if update == "config" {
				latestConfig = quotaConfigCopy(svc.lastConfig)
				latestConfig.ServerPort++
				deliverQuotaConfig(svc, "WS", latestConfig)
			} else {
				latestUsers = []model.UserSpec{{ID: 2, UUID: "latest-fixture-user"}}
				svc.handleWSEvent(ctx, controlplane.Event{Type: controlplane.EventSyncUsers, Users: latestUsers})
			}
			close(source.release)
			select {
			case result := <-svc.pullResults:
				svc.applyPullResult(ctx, result)
			case <-time.After(5 * time.Second):
				t.Fatal("REST poll did not complete")
			}
			if svc.lastConfig.ServerPort != latestConfig.ServerPort || svc.lastUsers[0].UUID != latestUsers[0].UUID {
				t.Fatal("late REST snapshot rolled back a newer push")
			}
			if !svc.wsResyncPending.Load() || source.resyncRequests != 1 {
				t.Fatal("discarding an old snapshot must force a fresh reconciliation")
			}
			if update == "config" && k.reloadCalls != 1 || update == "users" && k.updateCalls != 1 {
				t.Fatal("late REST snapshot triggered an extra kernel update")
			}
		})
	}
}

func TestQuotaUpdateStartsProtocolWhenFirstUserDeltaArrives(t *testing.T) {
	svc, k, _ := quotaReloadService(t)
	k.running = false
	svc.lastUsers = nil
	users := []model.UserSpec{{ID: 2, UUID: "first-delta-fixture-user"}}
	svc.applyUserDelta(context.Background(), "add", users)
	if !k.IsRunning() || k.startCalls != 1 || k.addCalls != 0 || len(svc.lastUsers) != 1 {
		t.Fatal("first user delta must start the cached protocol with the new users")
	}
}

func TestPullRemainsActiveUntilResultApplied(t *testing.T) {
	svc, _, _ := quotaReloadService(t)
	svc.source = quotaPollSource{snapshot: controlplane.Snapshot{Config: quotaConfigCopy(svc.lastConfig)}}
	svc.pullResults = make(chan pullResult, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	svc.pullViaAPIAsync(ctx)
	select {
	case result := <-svc.pullResults:
		time.Sleep(10 * time.Millisecond)
		if !svc.pullActive.Load() {
			t.Fatal("poll lock was released before queued state was applied")
		}
		svc.pullViaAPIAsync(ctx)
		select {
		case <-svc.pullResults:
			t.Fatal("a second poll was queued before applying the first")
		case <-time.After(20 * time.Millisecond):
		}
		svc.applyPullResult(ctx, result)
		if svc.pullActive.Load() {
			t.Fatal("poll lock was not released after applying state")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("poll did not produce a result")
	}
}

func TestUserRemovalBeforeConfigArrivalUpdatesCache(t *testing.T) {
	svc, k, _ := quotaReloadService(t)
	k.running = false
	svc.lastConfig = nil
	users := append([]model.UserSpec(nil), svc.lastUsers...)
	svc.applyUserDelta(context.Background(), "remove", users)
	if len(svc.lastUsers) != 0 || k.startCalls != 0 || k.removeCalls != 0 {
		t.Fatal("user removal before startup must update the cache without touching a stopped kernel")
	}
}

type reconnectQuotaSource struct {
	controlplane.Source
	snapshot       controlplane.Snapshot
	resyncRequests int
}

func (s *reconnectQuotaSource) SupportsPolling() bool { return true }
func (s *reconnectQuotaSource) RequestResync()        { s.resyncRequests++ }
func (s *reconnectQuotaSource) Poll(context.Context) (controlplane.Snapshot, error) {
	return s.snapshot, nil
}

func TestReconnectCannotBeSatisfiedByAlreadyQueuedRESTSnapshot(t *testing.T) {
	for _, event := range []string{"reconnect", "drop"} {
		t.Run(event, func(t *testing.T) {
			svc, _, _ := quotaReloadService(t)
			svc.wsClient = connectedQuotaPush{}
			originalConfig := svc.lastConfig
			originalUsers := append([]model.UserSpec(nil), svc.lastUsers...)
			source := &reconnectQuotaSource{snapshot: controlplane.Snapshot{Config: originalConfig, Users: originalUsers}}
			svc.source = source
			svc.pullResults = make(chan pullResult, 1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			svc.pullViaAPIAsync(ctx)
			var queued pullResult
			select {
			case queued = <-svc.pullResults:
			case <-time.After(5 * time.Second):
				t.Fatal("initial REST snapshot did not queue")
			}
			latestConfig := quotaConfigCopy(originalConfig)
			latestConfig.ServerPort++
			latestUsers := []model.UserSpec{{ID: 2, UUID: "post-reconnect-fixture-user"}}
			source.snapshot = controlplane.Snapshot{Config: latestConfig, Users: latestUsers}
			svc.handleWSStatus(ctx, controlplane.StatusChange{Connected: event == "reconnect", NeedsResync: event == "drop"})
			svc.applyPullResult(ctx, queued)
			if !svc.wsResyncPending.Load() || source.resyncRequests == 0 {
				t.Fatal("an old queued result consumed the newer reconciliation request")
			}
			if svc.lastConfig != originalConfig || svc.lastUserHash != computeUserHash(originalUsers) {
				t.Fatal("a pre-reconnect snapshot changed authoritative state")
			}
			// The pending flag keeps the regular ticker active even while WS is connected.
			svc.pullViaAPIAsync(ctx)
			select {
			case fresh := <-svc.pullResults:
				svc.applyPullResult(ctx, fresh)
			case <-time.After(5 * time.Second):
				t.Fatal("the newer reconciliation request did not fetch again")
			}
			if svc.wsResyncPending.Load() || svc.lastConfig != latestConfig || svc.lastUserHash != computeUserHash(latestUsers) {
				t.Fatal("fresh REST reconciliation did not apply missed updates")
			}
		})
	}
}
