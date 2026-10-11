package bandwidthcheck

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"math"
	"sync/atomic"
	"time"

	"github.com/micah123321/mi-node/internal/nlog"
)

// All runners in this process share one probe slot, including legacy nodes.
// The panel's lease also coordinates separate processes for the same scope.
var probeSlot = make(chan struct{}, 1)

type Runner struct {
	api                 API
	onState             func(State)
	ready               chan struct{}
	running             atomic.Bool
	initialDelay        time.Duration
	pollInterval        time.Duration
	unsupportedInterval time.Duration
	requestTimeout      time.Duration
	lastProbeID         string
}

func NewRunner(api API, onState func(State)) *Runner {
	var seed [8]byte
	_, _ = rand.Read(seed[:])
	return &Runner{
		api: api, onState: onState, ready: make(chan struct{}),
		initialDelay: time.Duration(binary.LittleEndian.Uint64(seed[:]) % uint64(30*time.Second)),
		pollInterval: time.Minute, unsupportedInterval: 15 * time.Minute,
		requestTimeout: 10 * time.Second,
	}
}

// Ready closes after the first policy request (including an unavailable panel).
// A legacy Service waits for it only during startup, before opening its inbound.
func (r *Runner) Ready() <-chan struct{} { return r.ready }

func (r *Runner) Run(ctx context.Context) {
	if !r.running.CompareAndSwap(false, true) {
		return
	}
	ready := false
	signalReady := func() {
		if !ready {
			close(r.ready)
			ready = true
		}
	}
	defer signalReady()
	if !wait(ctx, r.initialDelay) {
		return
	}
	for ctx.Err() == nil {
		pollStarted := time.Now()
		cfgCtx, cancel := context.WithTimeout(ctx, r.requestTimeout)
		cfg, err := r.api.Config(cfgCtx)
		cancel()
		delay := r.pollInterval
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			nlog.Core().Warn("bandwidth policy request failed", "error", err)
			if errors.Is(err, ErrUnsupported) {
				delay = r.unsupportedInterval
			}
			signalReady()
		} else {
			if r.onState != nil {
				r.onState(cfg.State)
			}
			signalReady()
			if cfg.Enabled && cfg.ProbeID != "" && cfg.ProbeID != r.lastProbeID {
				r.lastProbeID = cfg.ProbeID
				report := r.measure(ctx, cfg)
				if ctx.Err() != nil {
					return
				}
				if report.Error != nil {
					nlog.Core().Warn("bandwidth probe failed", "error", *report.Error, "confirmed_bytes", report.BytesReceived)
				} else {
					nlog.Core().Info("bandwidth probe completed", "confirmed_bytes", report.BytesReceived,
						"duration_ms", report.DurationMS, "confirmed_mbps", float64(report.BytesReceived)*8/(report.DurationMS*1000))
				}
				reportCtx, reportCancel := context.WithTimeout(ctx, r.requestTimeout)
				state, reportErr := r.api.Report(reportCtx, report)
				reportCancel()
				if reportErr != nil {
					// No local low-speed decisions: keep the last authoritative block.
					nlog.Core().Warn("bandwidth report failed; retaining panel state", "error", reportErr)
					if errors.Is(reportErr, ErrUnsupported) {
						delay = r.unsupportedInterval
					}
				} else if r.onState != nil {
					r.onState(state)
				}
			}
		}
		// Probe work consumes the polling period instead of postponing policy refresh.
		if !wait(ctx, max(delay-time.Since(pollStarted), 0)) {
			return
		}
	}
}

func (r *Runner) measure(ctx context.Context, cfg Config) Report {
	report := Report{ProbeID: cfg.ProbeID, PolicyRevision: cfg.PolicyRevision, ProbeMethod: cfg.ProbeMethod}
	fail := func(message string) Report { report.Error = &message; return report }
	cfg = defaults(cfg)
	if cfg.ProbeMethod != "" && cfg.ProbeMethod != CloudflareDownloadV1 {
		return fail("unsupported bandwidth probe method")
	}
	if cfg.PolicyRevision == "" || math.IsNaN(cfg.ThresholdMbps) || math.IsInf(cfg.ThresholdMbps, 0) ||
		cfg.ThresholdMbps <= 0 || cfg.ThresholdMbps > 100000 ||
		cfg.ChunkBytes < 1 || cfg.ChunkBytes > 1024*1024 || cfg.MaxBytes <= int64(cfg.ChunkBytes) || cfg.MaxBytes > 64*1024*1024 ||
		cfg.SampleSeconds < 1 || cfg.SampleSeconds > 60 || cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 120 {
		return fail("invalid bandwidth probe policy")
	}
	probeCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	select {
	case probeSlot <- struct{}{}:
		defer func() { <-probeSlot }()
	default:
		return fail("another bandwidth probe is active")
	}
	if cfg.ProbeMethod == CloudflareDownloadV1 {
		return measureDownload(probeCtx, cfg, report)
	}
	body := make([]byte, cfg.ChunkBytes)
	if _, err := rand.Read(body); err != nil {
		return fail("probe payload generation failed")
	}
	capBytesPerSecond := math.Max(cfg.ThresholdMbps*1.5, 12) * 1e6 / 8
	pace := func(n int) bool {
		return wait(probeCtx, time.Duration(float64(n)/capBytesPerSecond*float64(time.Second)))
	}
	if !pace(len(body)) {
		return fail("warmup canceled or timed out")
	}
	ack, err := r.api.Upload(probeCtx, cfg.ProbeID, true, body)
	if err != nil {
		return fail("warmup: " + err.Error())
	}
	if ack != int64(len(body)) {
		return fail("warmup acknowledgement mismatch")
	}

	// Warmup consumes the wire-byte and timeout budgets, but never measured bytes.
	remaining := cfg.MaxBytes - int64(len(body))
	started := time.Now()
	for remaining > 0 {
		n := min(int64(len(body)), remaining)
		ack, err = r.api.Upload(probeCtx, cfg.ProbeID, false, body[:int(n)])
		if err != nil {
			return fail("upload: " + err.Error())
		}
		if ack != n {
			return fail("upload acknowledgement mismatch")
		}
		report.BytesReceived += ack
		remaining -= n
		// The clock stops only after the panel response has been decoded and verified.
		elapsed := time.Since(started)
		// Fill only the pacing budget not already spent transferring and awaiting acknowledgement.
		target := time.Duration(float64(report.BytesReceived) / capBytesPerSecond * float64(time.Second))
		if elapsed < target {
			if !wait(probeCtx, target-elapsed) {
				return fail("measurement canceled or timed out")
			}
			elapsed = time.Since(started)
		}
		report.DurationMS = float64(elapsed) / float64(time.Millisecond)
		if elapsed >= time.Duration(cfg.SampleSeconds)*time.Second {
			break
		}
	}
	return report
}

func defaults(cfg Config) Config {
	if cfg.Interval == 0 {
		cfg.Interval = 3600
	}
	if cfg.RetryInterval == 0 {
		cfg.RetryInterval = 60
	}
	if cfg.RecoveryInterval == 0 {
		cfg.RecoveryInterval = 600
	}
	if cfg.ThresholdMbps == 0 {
		cfg.ThresholdMbps = 8
	}
	if cfg.SampleSeconds == 0 {
		cfg.SampleSeconds = 8
	}
	if cfg.ChunkBytes == 0 {
		cfg.ChunkBytes = 262144
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 8388608
		if cfg.ProbeMethod == CloudflareDownloadV1 {
			cfg.MaxBytes = cloudflareDownloadBytes
		}
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 60
		if cfg.ProbeMethod == CloudflareDownloadV1 {
			cfg.TimeoutSeconds = 120
		}
	}
	return cfg
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
