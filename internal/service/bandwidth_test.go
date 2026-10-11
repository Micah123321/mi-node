package service

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/bandwidthcheck"
	"github.com/micah123321/mi-node/internal/cert"
	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/limiter"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/trafficlimit"
)

type bandwidthKernel struct {
	fakeDebugKernel
	reloadCalls int
}

func (k *bandwidthKernel) Protocols() []string { return []string{"shadowsocks"} }
func (k *bandwidthKernel) Reload(*model.NodeSpec, []model.UserSpec, kernel.TLSCert) error {
	k.reloadCalls++
	return nil
}

type bandwidthBootstrap struct {
	controlplane.ControlPlane
	snapshot controlplane.Bootstrap
}

func (b bandwidthBootstrap) Initial(context.Context, func() map[string]interface{}, chan<- controlplane.Event, chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	return b.snapshot, nil
}

func bandwidthService(t *testing.T) (*Service, *bandwidthKernel) {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox", ConfigDir: dir}, Cert: config.CertConfig{CertMode: "none", CertDir: filepath.Join(dir, "certs")}}
	k := &bandwidthKernel{}
	l := limiter.New()
	return &Service{cfg: cfg, kernel: k, cert: cert.NewManager(cfg.Cert), limiter: l, speedTracker: limiter.NewSpeedTracker(l),
		trafficLimit: trafficlimit.New("", time.Now), lastConfig: &model.NodeSpec{Protocol: "shadowsocks", ServerPort: 12345}, lastUsers: []model.UserSpec{{ID: 1, UUID: "old"}}}, k
}

func TestBandwidthGuardInitialSetupKeepsControlPlane(t *testing.T) {
	svc, k := bandwidthService(t)
	bootstrap := controlplane.Bootstrap{Config: svc.lastConfig, Users: svc.lastUsers}
	svc.source = bandwidthBootstrap{snapshot: bootstrap}
	svc.lastConfig = nil
	svc.lastUsers = nil
	svc.applyBandwidthState(bandwidthcheck.State{Suspended: true, BlockService: true})
	if err := svc.initialSetup(context.Background()); err != nil {
		t.Fatalf("initialSetup: %v", err)
	}
	if k.startCalls != 0 || svc.lastConfig == nil || len(svc.lastUsers) != 1 {
		t.Fatalf("startup bypassed guard or discarded state: starts=%d", k.startCalls)
	}
	svc.applyBandwidthState(bandwidthcheck.State{})
	if k.startCalls != 1 || !k.IsRunning() {
		t.Fatal("authoritative recovery failed to start cached service")
	}
}

func TestBandwidthGuardCoversUserConfigAndRestartEntrypoints(t *testing.T) {
	svc, k := bandwidthService(t)
	k.running = true
	svc.applyBandwidthState(bandwidthcheck.State{Suspended: true, BlockService: true})
	if k.IsRunning() {
		t.Fatal("bandwidth block did not stop kernel")
	}
	if svc.startKernel(svc.lastConfig, svc.lastUsers) || svc.ensureRunning() {
		t.Fatal("start bypassed bandwidth guard")
	}
	svc.applyUserUpdate(context.Background(), []model.UserSpec{{ID: 2, UUID: "new"}}, "new-hash")
	svc.applyUserDelta(context.Background(), "add", []model.UserSpec{{ID: 3, UUID: "three"}})
	svc.applyUserDelta(context.Background(), "remove", []model.UserSpec{{ID: 2}})
	updated := &model.NodeSpec{Protocol: "shadowsocks", ServerPort: 23456}
	svc.handleWSEvent(context.Background(), controlplane.Event{Type: controlplane.EventSyncConfig, Config: updated})
	svc.applyPullResult(context.Background(), pullResult{config: updated, configHash: "updated", users: []model.UserSpec{{ID: 4, UUID: "four"}}, userHash: "four"})
	if k.startCalls != 0 || k.updateCalls != 0 || k.addCalls != 0 || k.removeCalls != 0 || k.reloadCalls != 0 {
		t.Fatalf("kernel modified under bandwidth guard: %+v", k)
	}
	if svc.lastConfig.ServerPort != 23456 || len(svc.lastUsers) != 1 || svc.lastUsers[0].ID != 4 {
		t.Fatal("control-plane updates not cached while paused")
	}
	svc.applyBandwidthState(bandwidthcheck.State{})
	if k.startCalls != 1 || svc.appliedState.Config.ServerPort != 23456 || svc.appliedState.Users[0].ID != 4 {
		t.Fatal("recovery did not apply latest config and users")
	}
}

func TestBandwidthAndQuotaRecoveryAreIndependent(t *testing.T) {
	for _, bandwidthFirst := range []bool{true, false} {
		name := "quota_resets_first"
		if bandwidthFirst {
			name = "bandwidth_recovers_first"
		}
		t.Run(name, func(t *testing.T) {
			svc, k := bandwidthService(t)
			k.running = true
			now := time.Date(2026, 4, 28, 12, 0, 0, 0, time.UTC)
			manager := trafficlimit.New("", func() time.Time { return now })
			svc.trafficLimit = manager
			action, err := manager.Configure(trafficlimit.Config{Enabled: true, Limit: 100, CurrentUsed: 100, NextResetAt: now.Add(time.Minute).Unix(), Timezone: "UTC"})
			if err != nil {
				t.Fatal(err)
			}
			svc.handleTrafficLimitAction(action)
			svc.applyBandwidthState(bandwidthcheck.State{Suspended: true, BlockService: true})
			clearBandwidth := func() { svc.applyBandwidthState(bandwidthcheck.State{}) }
			resetQuota := func() {
				now = now.Add(2 * time.Minute)
				action, err := manager.CheckReset()
				if err != nil {
					t.Fatal(err)
				}
				svc.handleTrafficLimitAction(action)
			}
			if bandwidthFirst {
				clearBandwidth()
			} else {
				resetQuota()
			}
			if k.IsRunning() || k.startCalls != 0 {
				t.Fatal("clearing one reason bypassed remaining block")
			}
			if bandwidthFirst {
				resetQuota()
			} else {
				clearBandwidth()
			}
			if !k.IsRunning() || k.startCalls != 1 {
				t.Fatalf("all reasons cleared but kernel did not resume: starts=%d", k.startCalls)
			}
		})
	}
}

func TestBandwidthScopeSuspensionDoesNotBlockAnUnaffectedNode(t *testing.T) {
	svc, k := bandwidthService(t)
	k.running = true
	svc.applyBandwidthState(bandwidthcheck.State{Suspended: true, BlockService: false})
	if svc.bandwidthBlocked || !k.IsRunning() {
		t.Fatal("scope suspension overrode node-specific block_service")
	}
}

func TestBandwidthRunnerStartupCancellation(t *testing.T) {
	svc, _ := bandwidthService(t)
	svc.cfg.Panel.URL = "http://127.0.0.1:1"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stop, err := svc.startBandwidthRunner(ctx)
	stop()
	if err != context.Canceled {
		t.Fatalf("startup cancellation: %v", err)
	}
}

func TestBandwidthMachineChildrenAndStandaloneDoNotCreateRunner(t *testing.T) {
	for _, cfg := range []*config.Config{
		{Panel: config.PanelConfig{URL: "http://127.0.0.1:1", MachineID: 7}},
		{Panel: config.PanelConfig{URL: "http://127.0.0.1:1"}, Standalone: &config.StandaloneConfig{Enabled: true}},
	} {
		svc := &Service{cfg: cfg}
		started := time.Now()
		stop, err := svc.startBandwidthRunner(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		stop()
		if svc.bandwidthStates != nil || time.Since(started) > time.Second {
			t.Fatal("unexpected panel runner for child or standalone")
		}
	}
}
