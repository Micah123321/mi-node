package service

import (
	"context"

	"github.com/micah123321/mi-node/internal/bandwidthcheck"
	"github.com/micah123321/mi-node/internal/nlog"
	"github.com/micah123321/mi-node/internal/panel"
)

func (s *Service) startBandwidthRunner(ctx context.Context) (func(), error) {
	noop := func() {}
	// Machine children share the Orchestrator's runner; standalone has no panel.
	if s.cfg.IsStandalone() || s.cfg.IsMachineMode() || s.cfg.Panel.MachineID > 0 || s.cfg.Panel.URL == "" {
		return noop, nil
	}
	if s.bandwidthStates == nil {
		s.bandwidthStates = make(chan bandwidthcheck.State, 1)
	}
	runCtx, cancel := context.WithCancel(ctx)
	client := panel.NewClient(s.cfg.Panel).NewBandwidthClient()
	runner := bandwidthcheck.NewRunner(client, func(state bandwidthcheck.State) {
		select {
		case s.bandwidthStates <- state:
		case <-runCtx.Done():
		}
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer client.Close()
		runner.Run(runCtx)
	}()
	stop := func() { cancel(); <-done }
	select {
	case <-runner.Ready():
	case <-ctx.Done():
	}
	if ctx.Err() != nil {
		return stop, ctx.Err()
	}
	// Startup is the only synchronous policy boundary. Once initialized, the
	// worker never blocks the heartbeat or mutates Service state directly.
	select {
	case state := <-s.bandwidthStates:
		s.applyBandwidthState(state)
	default:
	}
	return stop, nil
}

func (s *Service) applyBandwidthState(state bandwidthcheck.State) {
	wasBlocked := s.bandwidthBlocked
	s.bandwidthBlocked = state.BlockService
	if s.bandwidthBlocked {
		if s.kernel.IsRunning() {
			nlog.Core().Warn("stopping kernel because panel blocked node bandwidth")
			s.kernel.Stop()
		}
	} else if wasBlocked {
		nlog.Core().Info("panel cleared bandwidth block; checking remaining node policies")
		s.ensureRunning()
	}
}
