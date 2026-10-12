package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"testing"
	"time"
)

func TestUpdateBootstrapBackgroundAndActualConfig(t *testing.T) {
	old := bootstrapUpdateAgent
	called := make(chan string, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	bootstrapUpdateAgent = func(ctx context.Context, path string) error {
		defer close(done)
		if _, ok := ctx.Deadline(); !ok {
			t.Error("unbounded bootstrap")
		}
		called <- path
		<-release
		return errors.New("bootstrap fixture failed")
	}
	defer func() { close(release); <-done; bootstrapUpdateAgent = old }()
	returned := make(chan struct{})
	go func() { startUpdateBootstrap("/custom/node.yml"); close(returned) }()
	select {
	case <-returned:
	case <-time.After(time.Second):
		t.Fatal("bootstrap blocked business startup")
	}
	select {
	case path := <-called:
		if path != "/custom/node.yml" {
			t.Fatal("actual config path lost")
		}
	case <-time.After(time.Second):
		t.Fatal("bootstrap not started")
	}
}

func TestVersionProbesDoNotBootstrap(t *testing.T) {
	oldArgs, oldFlags, oldHook := os.Args, flag.CommandLine, bootstrapUpdateAgent
	defer func() { os.Args, flag.CommandLine, bootstrapUpdateAgent = oldArgs, oldFlags, oldHook }()
	bootstrapUpdateAgent = func(context.Context, string) error { t.Error("version probe started bootstrap"); return nil }
	for _, args := range [][]string{{"mi-node", "version", "--json"}, {"mi-node", "-v"}} {
		os.Args = args
		flag.CommandLine = flag.NewFlagSet("mi-node", flag.ContinueOnError)
		main()
	}
}
