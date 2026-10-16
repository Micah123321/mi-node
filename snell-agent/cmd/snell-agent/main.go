package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	agent "github.com/micah123321/mi-node/snell-agent"
)

func main() {
	path := flag.String("config", "config.json", "agent JSON configuration")
	check := flag.Bool("check-config", false, "validate configuration without starting the agent or touching report state")
	flag.Parse()
	logger := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := agent.LoadConfig(*path)
	if err != nil {
		logger.Error("configuration failed", "reason", err)
		os.Exit(1)
	}
	if *check {
		logger.Info("configuration is valid")
		return
	}
	a, err := agent.New(cfg, logger)
	if err != nil {
		logger.Error("agent initialization failed", "reason", err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("Snell TCP v4 agent started")
	if err = a.Run(ctx); err != nil {
		logger.Error("agent shutdown failed", "reason", err)
		os.Exit(1)
	}
	logger.Info("Snell agent stopped; pending reports retained")
}
