package service

import (
	"context"
	"errors"
	"testing"

	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/model"
)

type protocolReloadKernel struct {
	bandwidthKernel
	reloadErr   error
	stopOnError bool
	applied     *model.NodeSpec
}

func (k *protocolReloadKernel) Protocols() []string { return []string{"anytls", "mieru"} }
func (k *protocolReloadKernel) Reload(node *model.NodeSpec, _ []model.UserSpec, _ kernel.TLSCert) error {
	k.reloadCalls++
	if k.reloadErr != nil {
		if k.stopOnError {
			k.running = false
		}
		return k.reloadErr
	}
	k.applied = node
	return nil
}

func TestProtocolConfigUpdateRetriesAfterRollback(t *testing.T) {
	for _, transport := range []string{"REST", "WS", "REST304"} {
		t.Run(transport, func(t *testing.T) {
			svc, _ := bandwidthService(t)
			old := &model.NodeSpec{Protocol: "anytls", ServerPort: 12345, ServerName: "node.example.invalid"}
			if err := svc.ensureTLSCertificate(context.Background(), old); err != nil {
				t.Fatal(err)
			}
			k := &protocolReloadKernel{reloadErr: errors.New("target port is busy")}
			k.running, k.applied = true, old
			svc.kernel, svc.lastConfig, svc.appliedState.Config = k, old, old
			svc.appliedState.Users = svc.lastUsers
			svc.lastConfigHash = computeConfigHash(old)
			next := &model.NodeSpec{Protocol: "mieru", ServerPort: 12345, Transport: "TCP"}
			deliver := func() {
				if transport != "WS" {
					svc.applyPullResult(context.Background(), pullResult{config: next, configHash: computeConfigHash(next)})
				} else {
					svc.handleWSEvent(context.Background(), controlplane.Event{Type: controlplane.EventSyncConfig, Config: next})
				}
			}
			deliver()
			if k.reloadCalls != 1 || k.startCalls != 0 || !k.running || svc.appliedState.Config != old || !svc.configApplyPending {
				t.Fatalf("failed reload did not preserve old runtime and pending retry: reloads=%d starts=%d pending=%v", k.reloadCalls, k.startCalls, svc.configApplyPending)
			}
			k.reloadErr = nil
			if transport == "REST304" {
				svc.applyPullResult(context.Background(), pullResult{})
			} else {
				deliver()
			}
			if k.reloadCalls != 2 || k.applied != next || svc.appliedState.Config != next || svc.configApplyPending {
				t.Fatal("identical config did not retry and apply after recovery")
			}
			deliver()
			if k.reloadCalls != 2 {
				t.Fatal("successfully applied config reloaded again")
			}
		})
	}
}

func TestProtocolConfigUpdateRestartsStoppedKernel(t *testing.T) {
	svc, _ := bandwidthService(t)
	k := &protocolReloadKernel{reloadErr: errors.New("kernel stopped during reload"), stopOnError: true}
	k.running = true
	svc.kernel = k
	next := &model.NodeSpec{Protocol: "mieru", ServerPort: 12345, Transport: "TCP"}
	svc.handleWSEvent(context.Background(), controlplane.Event{Type: controlplane.EventSyncConfig, Config: next})
	if k.startCalls != 1 || !k.running || svc.appliedState.Config != next || svc.configApplyPending {
		t.Fatal("stopped kernel did not recover through Start")
	}
}
