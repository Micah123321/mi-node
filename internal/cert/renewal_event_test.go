package cert

import (
	"sync"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/config"
)

func TestRenewalEventsCoalesceWithoutBlocking(t *testing.T) {
	manager := NewManager(config.CertConfig{})
	var workers sync.WaitGroup
	for i := 0; i < 32; i++ {
		workers.Add(1)
		go func() { defer workers.Done(); manager.notifyRenewal() }()
	}
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("renewal producers blocked")
	}
	select {
	case <-manager.RenewalEvents():
	default:
		t.Fatal("renewal notification was lost")
	}
	if !manager.CertRenewed() || manager.CertRenewed() {
		t.Fatal("renewal flag must be consumed exactly once")
	}
	select {
	case <-manager.RenewalEvents():
		t.Fatal("notifications were not coalesced")
	default:
	}
	manager.notifyRenewal()
	select {
	case <-manager.RenewalEvents():
	default:
		t.Fatal("later renewal notification was lost")
	}
	if !manager.CertRenewed() {
		t.Fatal("later renewal flag was lost")
	}
}
