package bandwidthcheck

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

type testAPI struct {
	config func(context.Context) (Config, error)
	upload func(context.Context, string, bool, []byte) (int64, error)
	report func(context.Context, Report) (State, error)
}

func (a testAPI) Config(ctx context.Context) (Config, error) { return a.config(ctx) }
func (a testAPI) Upload(ctx context.Context, id string, warmup bool, b []byte) (int64, error) {
	return a.upload(ctx, id, warmup, b)
}
func (a testAPI) Report(ctx context.Context, result Report) (State, error) {
	return a.report(ctx, result)
}

func probeConfig() Config {
	return Config{Enabled: true, ProbeID: "probe", PolicyRevision: "revision", ThresholdMbps: 8,
		SampleSeconds: 8, ChunkBytes: 32768, MaxBytes: 3 * 32768, TimeoutSeconds: 2}
}

func fastRunner(api API, onState func(State)) *Runner {
	r := NewRunner(api, onState)
	r.initialDelay = 0
	r.pollInterval = 5 * time.Millisecond
	return r
}

func TestMeasureConfirmedBytesAndAcknowledgementTime(t *testing.T) {
	cfg := probeConfig()
	cfg.MaxBytes -= 17 // Exercise a final chunk smaller than chunk_bytes.
	var wireBytes, warmups, uploads int
	api := testAPI{upload: func(ctx context.Context, id string, warmup bool, body []byte) (int64, error) {
		if id != cfg.ProbeID {
			t.Errorf("probe id = %q", id)
		}
		if len(body) > cfg.ChunkBytes {
			t.Errorf("oversize chunk: %d", len(body))
		}
		wireBytes += len(body)
		if warmup {
			warmups++
		} else {
			uploads++
			if !wait(ctx, 60*time.Millisecond) {
				return 0, ctx.Err()
			}
		}
		return int64(len(body)), nil
	}}
	result := NewRunner(api, nil).measure(context.Background(), cfg)
	if result.Error != nil {
		t.Fatalf("measure: %s", *result.Error)
	}
	if warmups != 1 || uploads != 2 || wireBytes != int(cfg.MaxBytes) {
		t.Fatalf("requests: warmup=%d measure=%d wire=%d", warmups, uploads, wireBytes)
	}
	if result.BytesReceived != cfg.MaxBytes-int64(cfg.ChunkBytes) {
		t.Fatalf("measured bytes include warmup: %d", result.BytesReceived)
	}
	if result.DurationMS < 120 {
		t.Fatalf("acknowledgement delay omitted: %.2f ms", result.DurationMS)
	}
	if mbps := float64(result.BytesReceived) * 8 / (result.DurationMS * 1000); mbps >= cfg.ThresholdMbps {
		t.Fatalf("expected slow acknowledged transfer: %.2f Mbps", mbps)
	}
}

func TestMeasureThresholdAndPacing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ackDelay time.Duration
		low      bool
	}{
		{"above", 0, false}, {"below", 60 * time.Millisecond, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := probeConfig()
			cfg.MaxBytes = 2 * int64(cfg.ChunkBytes)
			api := testAPI{upload: func(ctx context.Context, _ string, warmup bool, b []byte) (int64, error) {
				if !warmup && !wait(ctx, tc.ackDelay) {
					return 0, ctx.Err()
				}
				return int64(len(b)), nil
			}}
			result := NewRunner(api, nil).measure(context.Background(), cfg)
			if result.Error != nil {
				t.Fatalf("measure: %s", *result.Error)
			}
			mbps := float64(result.BytesReceived) * 8 / (result.DurationMS * 1000)
			if (mbps < cfg.ThresholdMbps) != tc.low {
				t.Fatalf("threshold result %.2f Mbps, want low=%v", mbps, tc.low)
			}
			if mbps > math.Max(cfg.ThresholdMbps*1.5, 12)*1.05 {
				t.Fatalf("pacing exceeded cap: %.2f Mbps", mbps)
			}
		})
	}
}

func TestMeasurePacingDoesNotPenalizeAnUplinkAlreadyBelowThePacingCap(t *testing.T) {
	cfg := probeConfig()
	cfg.ChunkBytes = 262144
	cfg.MaxBytes = 3 * int64(cfg.ChunkBytes)
	transferDelay := time.Duration(float64(cfg.ChunkBytes) * 8 / 10e6 * float64(time.Second))
	api := testAPI{upload: func(ctx context.Context, _ string, warmup bool, body []byte) (int64, error) {
		if !warmup && !wait(ctx, transferDelay) {
			return 0, ctx.Err()
		}
		return int64(len(body)), nil
	}}
	result := NewRunner(api, nil).measure(context.Background(), cfg)
	if result.Error != nil {
		t.Fatalf("measure: %s", *result.Error)
	}
	mbps := float64(result.BytesReceived) * 8 / (result.DurationMS * 1000)
	if mbps < cfg.ThresholdMbps {
		t.Fatalf("healthy 10 Mbps uplink became %.2f Mbps after pacing", mbps)
	}
}

func TestMeasureErrorsAreNotZeroSpeedSamples(t *testing.T) {
	for _, tc := range []struct {
		name     string
		warmup   bool
		mismatch bool
	}{
		{"warmup_http", true, false}, {"measurement_http", false, false}, {"ack_mismatch", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := testAPI{upload: func(_ context.Context, _ string, warmup bool, b []byte) (int64, error) {
				if warmup == tc.warmup {
					if tc.mismatch {
						return int64(len(b) - 1), nil
					}
					return 0, errors.New("HTTP 503")
				}
				return int64(len(b)), nil
			}}
			result := NewRunner(api, nil).measure(context.Background(), probeConfig())
			if result.Error == nil || *result.Error == "" {
				t.Fatal("failed transfer presented as a speed sample")
			}
			if result.BytesReceived != 0 {
				t.Fatalf("unconfirmed bytes counted: %d", result.BytesReceived)
			}
		})
	}
}

func TestMeasureStopsAtSampleWindow(t *testing.T) {
	cfg := probeConfig()
	cfg.SampleSeconds = 1
	cfg.MaxBytes = 20 * int64(cfg.ChunkBytes)
	var measured int
	api := testAPI{upload: func(ctx context.Context, _ string, warmup bool, b []byte) (int64, error) {
		if !warmup {
			measured++
			if !wait(ctx, 550*time.Millisecond) {
				return 0, ctx.Err()
			}
		}
		return int64(len(b)), nil
	}}
	result := NewRunner(api, nil).measure(context.Background(), cfg)
	if result.Error != nil || measured != 2 || result.DurationMS < 1000 || result.BytesReceived != 2*int64(cfg.ChunkBytes) {
		t.Fatalf("sample window ignored: measured=%d result=%+v", measured, result)
	}
}

func TestMeasureTimeoutIncludesWarmup(t *testing.T) {
	cfg := probeConfig()
	cfg.TimeoutSeconds = 1
	api := testAPI{upload: func(ctx context.Context, _ string, warmup bool, b []byte) (int64, error) {
		delay := 700 * time.Millisecond
		if warmup {
			delay = 500 * time.Millisecond
		}
		if !wait(ctx, delay) {
			return 0, ctx.Err()
		}
		return int64(len(b)), nil
	}}
	started := time.Now()
	result := NewRunner(api, nil).measure(context.Background(), cfg)
	if result.Error == nil || result.BytesReceived != 0 {
		t.Fatalf("timeout result: %+v", result)
	}
	if elapsed := time.Since(started); elapsed > 1500*time.Millisecond {
		t.Fatalf("warmup excluded from timeout: %v", elapsed)
	}
}

func TestProcessProbeSerializationAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	done := make(chan Report, 1)
	api := testAPI{upload: func(ctx context.Context, _ string, _ bool, _ []byte) (int64, error) {
		close(entered)
		<-ctx.Done()
		return 0, ctx.Err()
	}}
	go func() { done <- NewRunner(api, nil).measure(ctx, probeConfig()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("probe never entered upload")
	}
	other := NewRunner(testAPI{upload: func(context.Context, string, bool, []byte) (int64, error) {
		t.Error("concurrent upload")
		return 0, nil
	}}, nil).measure(ctx, probeConfig())
	if other.Error == nil {
		t.Fatal("overlapping probe admitted")
	}
	cancel()
	select {
	case result := <-done:
		if result.Error == nil {
			t.Fatal("cancellation not reported")
		}
	case <-time.After(time.Second):
		t.Fatal("probe ignored cancellation")
	}
	// Cancellation must release the shared slot for the next node.
	next := NewRunner(testAPI{upload: func(_ context.Context, _ string, _ bool, b []byte) (int64, error) { return int64(len(b)), nil }}, nil).measure(context.Background(), probeConfig())
	if next.Error != nil {
		t.Fatalf("slot leaked: %s", *next.Error)
	}
}

func TestRunnerAppliesOnlyAuthoritativeReportState(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	states := make(chan State, 4)
	reports := make(chan Report, 1)
	cfg := probeConfig()
	cfg.State = State{Suspended: true, BlockService: true}
	api := testAPI{
		config: func(context.Context) (Config, error) { return cfg, nil },
		upload: func(_ context.Context, _ string, _ bool, b []byte) (int64, error) { return int64(len(b)), nil },
		report: func(_ context.Context, result Report) (State, error) {
			reports <- result
			return State{Suspended: false, BlockService: false, NextCheckAt: 123}, nil
		},
	}
	runner := fastRunner(api, func(state State) { states <- state })
	done := make(chan struct{})
	go func() { runner.Run(ctx); close(done) }()
	first := <-states
	if !first.BlockService {
		t.Fatal("initial authoritative block lost")
	}
	select {
	case state := <-states:
		if state.BlockService || state.NextCheckAt != 123 {
			t.Fatalf("report state ignored: %+v", state)
		}
	case <-time.After(time.Second):
		t.Fatal("missing report state")
	}
	result := <-reports
	if result.Error != nil || result.BytesReceived == 0 {
		t.Fatalf("bad successful report: %+v", result)
	}
	cancel()
	<-done
}

func TestRunnerRetainsBlockOnReportFailureAndSkipsDuplicateLease(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var configs, reports atomic.Int32
	cfg := probeConfig()
	cfg.State = State{Suspended: true, BlockService: true}
	states := make(chan State, 8)
	api := testAPI{
		config: func(context.Context) (Config, error) { configs.Add(1); return cfg, nil },
		upload: func(context.Context, string, bool, []byte) (int64, error) { return 0, errors.New("HTTP 500") },
		report: func(_ context.Context, result Report) (State, error) {
			if result.Error == nil {
				t.Error("HTTP failure was reported as 0 Mbps")
			}
			reports.Add(1)
			return State{}, errors.New("report offline")
		},
	}
	done := make(chan struct{})
	go func() { fastRunner(api, func(state State) { states <- state }).Run(ctx); close(done) }()
	for i := 0; i < 3; i++ {
		select {
		case state := <-states:
			if !state.BlockService {
				t.Fatal("report failure cleared block")
			}
		case <-time.After(time.Second):
			t.Fatal("policy polling stalled")
		}
	}
	cancel()
	<-done
	if configs.Load() < 3 || reports.Load() != 1 {
		t.Fatalf("lease repeated or no polling: configs=%d reports=%d", configs.Load(), reports.Load())
	}
}

func TestRunnerOldPanelRetriesAtLowFrequency(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	api := testAPI{config: func(context.Context) (Config, error) { calls.Add(1); return Config{}, ErrUnsupported }}
	r := fastRunner(api, func(State) { t.Error("404 must not change node state") })
	r.unsupportedInterval = 100 * time.Millisecond
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	<-r.Ready()
	wait(context.Background(), 40*time.Millisecond)
	if calls.Load() != 1 {
		t.Fatalf("old panel polled too often: %d", calls.Load())
	}
	cancel()
	<-done
}

func TestRunnerDisabledScopeDoesNotUpload(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	api := testAPI{
		config: func(context.Context) (Config, error) { cfg := probeConfig(); cfg.Enabled = false; return cfg, nil },
		upload: func(context.Context, string, bool, []byte) (int64, error) {
			t.Error("disabled scope uploaded")
			return 0, nil
		},
	}
	done := make(chan struct{})
	r := fastRunner(api, func(State) {})
	go func() { r.Run(ctx); close(done) }()
	<-r.Ready()
	cancel()
	<-done
}
