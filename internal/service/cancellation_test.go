package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/controlplane"
)

type canceledBootstrap struct {
	controlplane.LocalControlPlane
	ready     chan struct{}
	result    error
	immediate bool
}

func (s *canceledBootstrap) Initial(ctx context.Context, _ func() map[string]interface{}, _ chan<- controlplane.Event, _ chan<- controlplane.StatusChange) (controlplane.Bootstrap, error) {
	close(s.ready)
	if !s.immediate {
		<-ctx.Done()
	}
	return controlplane.Bootstrap{}, s.result
}

func TestReloadCancellationDuringStartupIsNormal(t *testing.T) {
	genuine := errors.New("certificate storage failed")
	for _, tc := range []struct {
		name   string
		err    error
		normal bool
	}{
		{"canceled", context.Canceled, true},
		{"wrapped", fmt.Errorf("initial snapshot: %w", context.Canceled), true},
		{"joined_cancel", errors.Join(context.Canceled, fmt.Errorf("request: %w", context.Canceled)), true},
		{"real_failure", genuine, false},
		{"joined_real", errors.Join(context.Canceled, genuine), false},
		{"deadline", context.DeadlineExceeded, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox", ConfigDir: dir}, Cert: config.CertConfig{CertMode: "none", CertDir: filepath.Join(dir, "certs")}}
			source := &canceledBootstrap{ready: make(chan struct{}), result: tc.err}
			svc := NewWithControlPlane(cfg, source)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- svc.Run(ctx) }()
			select {
			case <-source.ready:
			case <-time.After(3 * time.Second):
				t.Fatal("startup never reached initial snapshot")
			}
			cancel()
			select {
			case err := <-done:
				if tc.normal && err != nil {
					t.Fatalf("reload cancellation treated as failure: %v", err)
				}
				if !tc.normal && !errors.Is(err, tc.err) {
					t.Fatalf("startup failure discarded: %v", err)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("reload blocked during startup cancellation")
			}
		})
	}
}

func TestCancellationOnlyPreservesNilAndOtherFailures(t *testing.T) {
	for _, err := range []error{nil, context.DeadlineExceeded, errors.New("storage failure"), errors.Join(context.Canceled, errors.New("report failed"))} {
		if cancellationOnly(err) {
			t.Fatalf("genuine result classified as cancellation: %v", err)
		}
	}
}

func TestUnrequestedStartupCancellationIsStillFailure(t *testing.T) {
	cfg := &config.Config{Kernel: config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir()}, Cert: config.CertConfig{CertMode: "none", CertDir: t.TempDir()}}
	source := &canceledBootstrap{ready: make(chan struct{}), result: context.Canceled, immediate: true}
	if err := NewWithControlPlane(cfg, source).Run(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("unrequested cancellation discarded: %v", err)
	}
}

func TestReloadCancelsBandwidthStartupNormally(t *testing.T) {
	cfg := &config.Config{Panel: config.PanelConfig{URL: "http://127.0.0.1:1"}, Kernel: config.KernelConfig{Type: "singbox", ConfigDir: t.TempDir()}, Cert: config.CertConfig{CertMode: "none", CertDir: t.TempDir()}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := New(cfg).Run(ctx); err != nil {
		t.Fatalf("bandwidth startup cancellation: %v", err)
	}
}
