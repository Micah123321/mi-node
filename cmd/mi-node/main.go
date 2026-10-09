package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/machine"
	"github.com/micah123321/mi-node/internal/nlog"
	"github.com/micah123321/mi-node/internal/panel"
	"github.com/micah123321/mi-node/internal/readiness"
	"github.com/micah123321/mi-node/internal/service"
	"github.com/micah123321/mi-node/internal/updateagent"
)

var (
	version   = "dev"
	buildTime = "unknown"
)

type serviceRegistry struct {
	mu       sync.RWMutex
	services []*service.Service
}

func (r *serviceRegistry) Set(services []*service.Service) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.services = append([]*service.Service(nil), services...)
}

func (r *serviceRegistry) SnapshotDebugStatuses() []service.EgressDebugStatus {
	r.mu.RLock()
	services := append([]*service.Service(nil), r.services...)
	r.mu.RUnlock()

	statuses := make([]service.EgressDebugStatus, 0, len(services))
	for _, svc := range services {
		if svc == nil {
			continue
		}
		statuses = append(statuses, svc.DebugEgressStatus())
	}
	return statuses
}

func newHealthMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	return mux
}

func newDebugMux(statuses func() []service.EgressDebugStatus) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/egress", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")

		resp := struct {
			Nodes []service.EgressDebugStatus `json:"nodes"`
		}{
			Nodes: statuses(),
		}

		if err := json.NewEncoder(w).Encode(resp); err != nil {
			nlog.Core().Warn("failed to write debug response", "error", err)
		}
	})
	return mux
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "version" && os.Args[2] == "--json" {
		_ = updateagent.PrintVersion(version)
		return
	}
	configPath := flag.String("c", "config.yml", "config file path")
	showVersion := flag.Bool("v", false, "show version")
	flag.Parse()

	if *showVersion {
		fmt.Printf("mi-node %s (built %s)\n", version, buildTime)
		os.Exit(0)
	}

	rootCfg, err := config.LoadRoot(*configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to load config: %v\n", err)
		os.Exit(1)
	}

	instances, err := rootCfg.NormalizeInstances()
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to normalize config: %v\n", err)
		os.Exit(1)
	}
	if len(instances) == 0 {
		fmt.Fprintln(os.Stderr, "no runnable instances configured")
		os.Exit(1)
	}

	config.InitLogger(instances[0].Log)
	if err := panel.InitUpdateInventory(version, ""); err != nil {
		nlog.Core().Warn("update inventory discovery disabled", "error", err)
	}
	applyRuntimeConfig(instances[0].Runtime)

	runWithReload(rootCfg, *configPath)
}

func runWithReload(initialRoot *config.RootConfig, configPath string) {
	var healthSrv *http.Server
	var healthPort int
	var debugSrv *http.Server
	var debugPort int
	var registry serviceRegistry

	startHealth := func(port int) {
		if port <= 0 {
			healthPort = 0
			return
		}
		ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
		if err != nil {
			nlog.Core().Error("failed to start health check listener", "port", port, "error", err)
			os.Exit(1)
		}
		healthSrv = &http.Server{Handler: newHealthMux(), ReadHeaderTimeout: 5 * time.Second}
		healthPort = port
		go func() {
			nlog.Core().Info("health check listening", "addr", ln.Addr())
			if err := healthSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				nlog.Core().Warn("health check server stopped", "error", err)
			}
		}()
	}

	startDebug := func(port int) {
		if port <= 0 {
			debugPort = 0
			return
		}
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			nlog.Core().Error("failed to start local debug listener", "port", port, "error", err)
			os.Exit(1)
		}
		debugSrv = &http.Server{Handler: newDebugMux(registry.SnapshotDebugStatuses), ReadHeaderTimeout: 5 * time.Second}
		debugPort = port
		go func() {
			nlog.Core().Info("local debug listening", "addr", ln.Addr())
			if err := debugSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
				nlog.Core().Warn("local debug server stopped", "error", err)
			}
		}()
	}

	closeHealth := func() {
		if healthSrv != nil {
			_ = healthSrv.Close()
			healthSrv = nil
		}
		healthPort = 0
	}

	closeDebug := func() {
		if debugSrv != nil {
			_ = debugSrv.Close()
			debugSrv = nil
		}
		debugPort = 0
	}

	defer closeHealth()
	defer closeDebug()

	for root := initialRoot; ; {
		instances, err := root.NormalizeInstances()
		if err != nil {
			nlog.Core().Error("failed to normalize config", "error", err)
			os.Exit(1)
		}
		if len(instances) == 0 {
			nlog.Core().Error("no runnable instances configured")
			os.Exit(1)
		}
		if err := config.ValidateStartupLayout(instances); err != nil {
			nlog.Core().Error("startup layout validation failed", "error", err)
			os.Exit(1)
		}

		if instances[0].HealthPort != healthPort {
			closeHealth()
			startHealth(instances[0].HealthPort)
		}
		if instances[0].DebugPort != debugPort {
			closeDebug()
			startDebug(instances[0].DebugPort)
		}

		nlog.Core().Info("mi-node starting",
			"version", version,
			"build_time", buildTime,
			"instances", len(instances),
		)

		ctx, cancel := context.WithCancel(context.Background())
		ctx, readyRegistry := readiness.New(ctx, version)
		for _, cfg := range instances {
			if cfg.IsMachineMode() {
				readiness.Set(ctx, readiness.Key(cfg.InstanceID, cfg.Panel.URL, 0), false)
			} else {
				for _, node := range cfg.ExpandNodes() {
					readiness.Set(ctx, readiness.Key(node.InstanceID, node.Panel.URL, node.Panel.NodeID), false)
				}
			}
		}
		if err := readyRegistry.Publish(ctx, updateagent.ReadinessPath); err != nil {
			nlog.Core().Warn("readiness unavailable", "error", err)
		}
		reloadCh := make(chan *config.RootConfig, 1)

		watcher, err := config.WatchConfigRoot(ctx, configPath, func(newRoot *config.RootConfig) {
			select {
			case reloadCh <- newRoot:
			default:
			}
		})
		if err != nil {
			nlog.Core().Warn("config watcher unavailable, hot-reload disabled", "error", err)
		}

		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

		go func() {
			select {
			case sig := <-sigCh:
				nlog.Core().Info("received signal, shutting down gracefully", "signal", sig)
				cancel()

				select {
				case sig = <-sigCh:
					nlog.Core().Warn("received second signal, forcing exit", "signal", sig)
					os.Exit(1)
				case <-time.After(service.ShutdownReportTimeout + 30*time.Second):
					nlog.Core().Error("shutdown timed out after 120s, forcing exit")
					os.Exit(2)
				}
			case <-ctx.Done():
			}
		}()

		errCh := make(chan error, len(instances))
		var wg sync.WaitGroup
		services := make([]*service.Service, 0, len(instances))

		for _, instanceCfg := range instances {
			instanceCfg := instanceCfg
			var nodeServices []*service.Service
			if !instanceCfg.IsMachineMode() {
				nodes := instanceCfg.ExpandNodes()
				nodeServices = make([]*service.Service, 0, len(nodes))
				for _, nodeCfg := range nodes {
					svc := service.New(nodeCfg)
					nodeServices = append(nodeServices, svc)
					services = append(services, svc)
				}
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				if instanceCfg.IsMachineMode() {
					nlog.Core().Info("starting machine instance",
						"instance", instanceCfg.InstanceID,
						"machine_id", instanceCfg.Machine.MachineID,
						"panel_url", instanceCfg.Panel.URL,
					)
					orch := machine.New(instanceCfg)
					if err := orch.Run(ctx); err != nil {
						nlog.Core().Error("machine instance exited with error",
							"instance", instanceCfg.InstanceID,
							"error", err,
						)
						select {
						case errCh <- err:
						default:
						} // Retain at least one failure without blocking other shutdowns.
						cancel()
					}
					return
				}

				nodes := instanceCfg.ExpandNodes()
				nlog.Core().Info("starting node instance",
					"instance", instanceCfg.InstanceID,
					"nodes", len(nodes),
					"panel_url", instanceCfg.Panel.URL,
				)

				var instanceWG sync.WaitGroup
				for idx, nodeCfg := range nodes {
					nodeCfg := nodeCfg
					svc := nodeServices[idx]
					instanceWG.Add(1)
					go func(idx int, svc *service.Service) {
						defer instanceWG.Done()
						if idx > 0 {
							delay := time.Duration(idx) * 250 * time.Millisecond
							if delay > 2*time.Second {
								delay = 2 * time.Second
							}
							select {
							case <-time.After(delay):
							case <-ctx.Done():
								return
							}
						}
						if err := svc.Run(ctx); err != nil {
							nlog.Core().Error("node service exited with error",
								"instance", nodeCfg.InstanceID,
								"node_id", nodeCfg.Panel.NodeID,
								"error", err,
							)
							select {
							case errCh <- err:
							default:
							} // Retain at least one failure without blocking other shutdowns.
							cancel()
						} else {
							nlog.Core().Info("node service stopped",
								"instance", nodeCfg.InstanceID,
								"node_id", nodeCfg.Panel.NodeID,
							)
						}
					}(idx, svc)
				}
				instanceWG.Wait()
			}()
		}
		registry.Set(services)

		doneCh := make(chan struct{})
		go func() {
			wg.Wait()
			close(doneCh)
		}()

		var newRoot *config.RootConfig
		select {
		case newRoot = <-reloadCh:
			nlog.Core().Info("config changed, restarting services")
			cancel()
			<-doneCh
		case <-doneCh:
		}

		signal.Stop(sigCh)
		if watcher != nil {
			watcher.Stop()
		}
		cancel()

		close(errCh)
		if err := firstError(errCh); err != nil {
			os.Exit(1)
		}
		if newRoot == nil {
			nlog.Core().Info("mi-node stopped")
			return
		}

		newInstances, err := newRoot.NormalizeInstances()
		if err != nil {
			nlog.Core().Error("failed to normalize reloaded config", "error", err)
			os.Exit(1)
		}
		if len(newInstances) == 0 {
			nlog.Core().Error("no runnable instances configured after reload")
			os.Exit(1)
		}
		config.InitLogger(newInstances[0].Log)
		applyRuntimeConfig(newInstances[0].Runtime)
		root = newRoot
		nlog.Core().Info("reload complete, services restarting with new config")
	}
}

func firstError(errCh <-chan error) error {
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// applyRuntimeConfig wires up Go runtime memory limits from the config file.
// Both settings can also be overridden by environment variables (GOMEMLIMIT /
// GOGC) before startup; this function applies config-file values at runtime.
func applyRuntimeConfig(rt config.RuntimeConfig) {
	if rt.GoGCPercent > 0 {
		prev := debug.SetGCPercent(rt.GoGCPercent)
		nlog.Core().Info("runtime: GOGC set", "gogc", rt.GoGCPercent, "prev", prev)
	}

	if rt.GoMemLimit != "" {
		limit, err := parseMemLimit(rt.GoMemLimit)
		if err != nil {
			nlog.Core().Warn("runtime: invalid gomemlimit, ignoring", "value", rt.GoMemLimit, "error", err)
		} else {
			prev := debug.SetMemoryLimit(limit)
			nlog.Core().Info("runtime: GOMEMLIMIT set",
				"limit", rt.GoMemLimit,
				"bytes", limit,
				"prev_bytes", prev,
			)
		}
	}
}

// parseMemLimit converts a human-readable size string to bytes.
// Supported suffixes: B, KiB, MiB, GiB, TiB (case-insensitive).
func parseMemLimit(s string) (int64, error) {
	s = strings.TrimSpace(s)
	suffixes := []struct {
		suffix string
		mult   int64
	}{
		{"TiB", 1 << 40},
		{"GiB", 1 << 30},
		{"MiB", 1 << 20},
		{"KiB", 1 << 10},
		{"B", 1},
	}
	upper := strings.ToUpper(s)
	for _, sf := range suffixes {
		if strings.HasSuffix(upper, strings.ToUpper(sf.suffix)) {
			numStr := strings.TrimSuffix(upper, strings.ToUpper(sf.suffix))
			numStr = strings.TrimSpace(numStr)
			var n int64
			if _, err := fmt.Sscanf(numStr, "%d", &n); err != nil {
				return 0, fmt.Errorf("parse number %q: %w", numStr, err)
			}
			return n * sf.mult, nil
		}
	}
	var n int64
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("unrecognised size format %q", s)
	}
	return n, nil
}
