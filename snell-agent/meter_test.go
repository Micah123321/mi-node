package agent

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestCancelledRateWaitsDoNotStrandOtherSessions(t *testing.T) {
	var p pacer
	p.set(1)
	ctx, cancel := context.WithCancel(context.Background())
	var workers sync.WaitGroup
	workers.Add(10)
	for i := 0; i < 10; i++ {
		go func() { defer workers.Done(); _ = p.wait(ctx, meterChunk) }()
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	done := make(chan struct{})
	go func() { workers.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("revoked rate waits did not cancel promptly")
	}
	start := time.Now()
	if e := p.wait(context.Background(), 64); e != nil {
		t.Fatal(e)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Fatal("cancelled sessions left future rate reservations behind")
	}
}
