package machine

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestStopAllWaitsAndReturnsNodeFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	o := &Orchestrator{nodes: map[int]*nodeHandle{1: {cancel: cancel, done: done}}}
	expected := errors.New("final report failed")
	go func() { <-ctx.Done(); o.mu.Lock(); o.nodeErr = expected; o.mu.Unlock(); close(done) }()
	result := make(chan error, 1)
	go func() { result <- o.stopAll() }()
	select {
	case err := <-result:
		if !errors.Is(err, expected) {
			t.Fatalf("stopAll error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("stopAll did not finish")
	}
}
