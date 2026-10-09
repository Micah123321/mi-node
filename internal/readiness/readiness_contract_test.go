package readiness

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/updateagent"
)

func TestContractRegistryRequiresAllBindings(t *testing.T) {
	ctx, r := New(context.Background(), "v1.1.0")
	if r.snapshot(false).Ready {
		t.Fatal("empty registry ready")
	}
	Set(ctx, "a", true)
	Set(ctx, "b", false)
	if r.snapshot(false).Ready {
		t.Fatal("partial registry ready")
	}
	Set(ctx, "b", true)
	s := r.snapshot(false)
	if !s.Ready || len(s.Bindings) != 2 || s.PID != os.Getpid() || s.BootID == "" || s.Version != "v1.1.0" {
		t.Fatalf("invalid readiness %+v", s)
	}
	s.Bindings["a"] = false
	if !r.snapshot(false).Bindings["a"] {
		t.Fatal("snapshot aliases mutable registry")
	}
	if r.snapshot(true).Ready {
		t.Fatal("stopped registry ready")
	}
	Set(ctx, "b", false)
	if r.snapshot(false).Ready {
		t.Fatal("lost binding still ready")
	}
	_, other := New(context.Background(), "v1.1.0")
	if other.snapshot(false).BootID == s.BootID {
		t.Fatal("boot IDs reused")
	}
	Set(context.Background(), "unregistered", true)
}
func TestContractBindingKeyDistinctAndOpaque(t *testing.T) {
	base := Key("instance", "https://panel.example/secret", 1)
	for _, other := range []string{Key("other", "https://panel.example/secret", 1), Key("instance", "https://other.example/secret", 1), Key("instance", "https://panel.example/secret", 2)} {
		if base == other {
			t.Fatal("binding identity collision")
		}
	}
	if strings.Contains(base, "secret") || strings.Contains(base, "panel.example") {
		t.Fatal("panel identity leaked")
	}
	if base != Key("instance", "https://panel.example/secret", 1) {
		t.Fatal("unstable binding ID")
	}
}
func TestContractRegistryConcurrentSnapshots(t *testing.T) {
	ctx, r := New(context.Background(), "v1.1.0")
	var wg sync.WaitGroup
	for _, id := range []string{"a", "b", "c", "d"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				Set(ctx, id, i%2 == 0)
				_ = r.snapshot(false)
			}
			Set(ctx, id, true)
		}(id)
	}
	wg.Wait()
	s := r.snapshot(false)
	if !s.Ready || len(s.Bindings) != 4 {
		t.Fatalf("lost bindings %+v", s)
	}
}
func TestContractPublishPersistsReadinessAndStoppedState(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx, r := New(base, "v1.1.0")
	Set(ctx, "a", true)
	Set(ctx, "b", true)
	path := filepath.Join(t.TempDir(), "run", "readiness.json")
	if err := r.Publish(ctx, path); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var initial updateagent.Readiness
	if err = json.Unmarshal(b, &initial); err != nil {
		t.Fatal(err)
	}
	if !initial.Ready || len(initial.Bindings) != 2 {
		t.Fatalf("invalid published state %+v", initial)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("permissions %o", info.Mode().Perm())
		}
	}
	cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err = os.ReadFile(path)
		var stopped updateagent.Readiness
		if err == nil && json.Unmarshal(b, &stopped) == nil && !stopped.Ready {
			if stopped.PID != initial.PID || stopped.BootID != initial.BootID {
				t.Fatal("stop changed identity")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("shutdown readiness was not persisted")
}
