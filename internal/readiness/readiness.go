// Package readiness records initialization facts, independent of panel liveness.
package readiness

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/micah123321/mi-node/internal/updateagent"
)

type key struct{}
type Registry struct {
	mu    sync.Mutex
	state updateagent.Readiness
}

func Key(instance, panel string, node int) string {
	return fmt.Sprintf("%x/%d", sha256.Sum256([]byte(instance+"|"+panel)), node)
}
func New(ctx context.Context, version string) (context.Context, *Registry) {
	r := &Registry{state: updateagent.Readiness{PID: os.Getpid(), BootID: updateagent.UUID(), Version: version, Bindings: map[string]bool{}}}
	return context.WithValue(ctx, key{}, r), r
}
func Set(ctx context.Context, id string, ready bool) {
	r, _ := ctx.Value(key{}).(*Registry)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.state.Bindings[id] = ready
}

// Remove is reserved for a confirmed configuration removal, never an unexpected exit.
func Remove(ctx context.Context, id string) {
	r, _ := ctx.Value(key{}).(*Registry)
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.state.Bindings, id)
}

func (r *Registry) snapshot(stopped bool) updateagent.Readiness {
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.state
	s.Bindings = map[string]bool{}
	s.Ready = !stopped && len(r.state.Bindings) > 0
	for k, v := range r.state.Bindings {
		s.Bindings[k] = v
		if !v {
			s.Ready = false
		}
	}
	return s
}
func (r *Registry) Publish(ctx context.Context, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := updateagent.AtomicJSON(path, r.snapshot(false)); err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				_ = updateagent.AtomicJSON(path, r.snapshot(true))
				return
			case <-ticker.C:
				_ = updateagent.AtomicJSON(path, r.snapshot(false))
			}
		}
	}()
	return nil
}
