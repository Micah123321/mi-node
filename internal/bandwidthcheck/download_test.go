package bandwidthcheck

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type downloadRoundTripper func(*http.Request) (*http.Response, error)

func (f downloadRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type downloadBody struct {
	ctx            context.Context
	remaining      int64
	bytesPerSecond float64
	pauseAfter     int64
	pause          time.Duration
	stall          bool
	terminalErr    error
	readBytes      int64
	closed         bool
}

func (b *downloadBody) Read(p []byte) (int, error) {
	if b.pause > 0 && b.readBytes >= b.pauseAfter {
		pause := b.pause
		b.pause = 0
		if !wait(b.ctx, pause) {
			return 0, b.ctx.Err()
		}
	}
	if b.remaining == 0 {
		if b.stall {
			<-b.ctx.Done()
			return 0, b.ctx.Err()
		}
		if b.terminalErr != nil {
			return 0, b.terminalErr
		}
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), b.remaining))
	if b.bytesPerSecond > 0 && !wait(b.ctx, time.Duration(float64(n)/b.bytesPerSecond*float64(time.Second))) {
		return 0, b.ctx.Err()
	}
	clear(p[:n])
	b.remaining -= int64(n)
	b.readBytes += int64(n)
	return n, nil
}

func (b *downloadBody) Close() error { b.closed = true; return nil }

func cloudflareConfig() Config {
	return Config{Enabled: true, ProbeID: "cloudflare-lease", PolicyRevision: "cf-v1", ThresholdMbps: 8,
		ProbeMethod: CloudflareDownloadV1, DownloadURL: CloudflareDownloadURL,
		SampleSeconds: 8, ChunkBytes: 262144, MaxBytes: 10000000, TimeoutSeconds: 120}
}

func downloadResponse(body io.ReadCloser) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/octet-stream"}},
		ContentLength: cloudflareDownloadBytes, Body: body}
}

func downloadRunner(t *testing.T) *Runner {
	t.Helper()
	return NewRunner(testAPI{upload: func(context.Context, string, bool, []byte) (int64, error) {
		t.Error("Cloudflare method uploaded a sample to the panel")
		return 0, errors.New("unexpected panel upload")
	}}, nil)
}

func assertDownloadError(t *testing.T, result Report) {
	t.Helper()
	if result.Error == nil || *result.Error == "" || result.ProbeMethod != CloudflareDownloadV1 {
		t.Fatalf("failure was not an unknown result with method: %+v", result)
	}
}

func TestDownloadExcludesTTFBAndWarmupAndHonorsByteBudget(t *testing.T) {
	cfg := cloudflareConfig()
	body := &downloadBody{remaining: cloudflareDownloadBytes + 1, pauseAfter: int64(cfg.ChunkBytes), pause: 10 * time.Millisecond}
	var requests int
	ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.Method != http.MethodGet || req.URL.String() != CloudflareDownloadURL || req.URL.User != nil || req.Body != nil {
			t.Errorf("unexpected download request: %s %s", req.Method, req.URL)
		}
		if req.Header.Get("Authorization") != "" || req.Header.Get("Cookie") != "" || req.Header.Get("Proxy-Authorization") != "" || len(req.URL.Query()) != 1 || req.Header.Get("Accept-Encoding") != "identity" {
			t.Error("download contains credentials or compression")
		}
		if !wait(req.Context(), 700*time.Millisecond) {
			return nil, req.Context().Err()
		}
		body.ctx = req.Context()
		return downloadResponse(body), nil
	}))
	started := time.Now()
	result := downloadRunner(t).measure(ctx, cfg)
	if result.Error != nil || result.ProbeMethod != CloudflareDownloadV1 || result.BytesReceived != cfg.MaxBytes-int64(cfg.ChunkBytes) {
		t.Fatalf("fast body result: %+v", result)
	}
	if time.Since(started) < 700*time.Millisecond || result.DurationMS >= 350 || result.DurationMS <= 0 {
		t.Fatalf("TTFB included or invalid body timing: %.3f ms", result.DurationMS)
	}
	if requests != 1 || body.readBytes != cfg.MaxBytes || !body.closed {
		t.Fatalf("wire budget or body lifecycle: requests=%d bytes=%d closed=%v", requests, body.readBytes, body.closed)
	}
}

func TestDownloadHealthy10MbpsHasNoPacingPenalty(t *testing.T) {
	body := &downloadBody{remaining: cloudflareDownloadBytes, bytesPerSecond: 10e6 / 8}
	ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		body.ctx = req.Context()
		return downloadResponse(body), nil
	}))
	cfg := cloudflareConfig()
	result := downloadRunner(t).measure(ctx, cfg)
	if result.Error != nil {
		t.Fatalf("healthy stream: %+v", result)
	}
	mbps := float64(result.BytesReceived) * 8 / (result.DurationMS * 1000)
	if mbps < cfg.ThresholdMbps || mbps > 11 {
		t.Fatalf("10 Mbps body measured as %.3f Mbps", mbps)
	}
	if !body.closed || body.readBytes > cfg.MaxBytes || body.readBytes != result.BytesReceived+int64(cfg.ChunkBytes) {
		t.Fatalf("download budget: wire=%d result=%+v", body.readBytes, result)
	}
}

func TestDownloadSlowContinuousStreamUsesEightSecondWindow(t *testing.T) {
	body := &downloadBody{remaining: cloudflareDownloadBytes, bytesPerSecond: 1e6 / 8}
	ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		body.ctx = req.Context()
		return downloadResponse(body), nil
	}))
	started := time.Now()
	result := downloadRunner(t).measure(ctx, cloudflareConfig())
	if result.Error != nil {
		t.Fatalf("continuous slow stream became unknown: %+v", result)
	}
	mbps := float64(result.BytesReceived) * 8 / (result.DurationMS * 1000)
	if mbps < 0.7 || mbps > 1.2 || result.DurationMS < 7900 || result.DurationMS > 8500 || time.Since(started) > 12*time.Second {
		t.Fatalf("8-second low sample: result=%+v mbps=%.3f elapsed=%v", result, mbps, time.Since(started))
	}
	if !body.closed || body.readBytes >= cloudflareDownloadBytes || body.ctx.Err() == nil {
		t.Fatalf("sample cutoff did not cancel/close early: bytes=%d closed=%v", body.readBytes, body.closed)
	}
}

func TestDownloadRejectsInvalidResponsesAndShortBodies(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*http.Response, *downloadBody)
	}{
		{"forbidden", func(r *http.Response, _ *downloadBody) { r.StatusCode = 403 }},
		{"rate_limited", func(r *http.Response, _ *downloadBody) { r.StatusCode = 429 }},
		{"server_error", func(r *http.Response, _ *downloadBody) { r.StatusCode = 503 }},
		{"html", func(r *http.Response, _ *downloadBody) { r.Header.Set("Content-Type", "text/html") }},
		{"gzip", func(r *http.Response, _ *downloadBody) { r.Header.Set("Content-Encoding", "gzip") }},
		{"already_decoded", func(r *http.Response, _ *downloadBody) { r.Uncompressed = true }},
		{"multiple_encodings", func(r *http.Response, _ *downloadBody) { r.Header["Content-Encoding"] = []string{"identity", "gzip"} }},
		{"wrong_length", func(r *http.Response, _ *downloadBody) { r.ContentLength-- }},
		{"short_warmup", func(_ *http.Response, b *downloadBody) { b.remaining = 100 }},
		{"short_body", func(_ *http.Response, b *downloadBody) { b.remaining = 262144 + 32768 }},
		{"undeclared_short_body", func(r *http.Response, b *downloadBody) { r.ContentLength = -1; b.remaining = 262144 + 32768 }},
		{"unexpected_eof", func(_ *http.Response, b *downloadBody) {
			b.remaining = 262144 + 32768
			b.terminalErr = io.ErrUnexpectedEOF
		}},
		{"read_timeout", func(_ *http.Response, b *downloadBody) {
			b.remaining = 262144 + 32768
			b.terminalErr = context.DeadlineExceeded
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &downloadBody{remaining: cloudflareDownloadBytes}
			ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
				body.ctx = req.Context()
				resp := downloadResponse(body)
				tc.mutate(resp, body)
				return resp, nil
			}))
			result := downloadRunner(t).measure(ctx, cloudflareConfig())
			assertDownloadError(t, result)
			if !body.closed {
				t.Fatal("error response body leaked")
			}
		})
	}
}

func TestDownloadTimeoutWithoutBodyIsUnknown(t *testing.T) {
	body := &downloadBody{stall: true}
	ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		body.ctx = req.Context()
		return downloadResponse(body), nil
	}))
	cfg := cloudflareConfig()
	cfg.TimeoutSeconds = 1
	started := time.Now()
	result := downloadRunner(t).measure(ctx, cfg)
	assertDownloadError(t, result)
	if result.BytesReceived != 0 || result.DurationMS != 0 || !body.closed || time.Since(started) > 1500*time.Millisecond {
		t.Fatalf("no-body timeout: %+v closed=%v", result, body.closed)
	}
}

func TestDownloadStalledSampleAndRealTimeoutRemainUnknown(t *testing.T) {
	for _, tc := range []struct {
		name    string
		burst   int64
		timeout bool
		pause   time.Duration
	}{
		{"no_measured_data", 0, false, 0}, {"burst_then_stall", 32768, false, 0}, {"overall_timeout", 32768, true, 0},
		{"late_burst_after_stall", 32768, false, 900 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := &downloadBody{remaining: 262144 + tc.burst, stall: true, pauseAfter: 262144, pause: tc.pause}
			ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
				body.ctx = req.Context()
				return downloadResponse(body), nil
			}))
			cfg := cloudflareConfig()
			if tc.timeout {
				cfg.TimeoutSeconds = 1
			} else {
				cfg.SampleSeconds = 1
			}
			result := downloadRunner(t).measure(ctx, cfg)
			assertDownloadError(t, result)
			if !body.closed {
				t.Fatal("stalled body leaked")
			}
		})
	}
}

func TestDownloadRejectsUnknownMethodsAndUntrustedPoliciesWithoutFallback(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Config)
	}{
		{"unknown_method", func(c *Config) { c.ProbeMethod = "future_method" }},
		{"panel_url", func(c *Config) { c.DownloadURL = "https://panel.example/__down?token=secret" }},
		{"http_url", func(c *Config) { c.DownloadURL = strings.Replace(CloudflareDownloadURL, "https:", "http:", 1) }},
		{"extra_query", func(c *Config) { c.DownloadURL += "&token=secret" }},
		{"oversize", func(c *Config) { c.MaxBytes++ }},
		{"undersize", func(c *Config) { c.MaxBytes-- }},
		{"oversize_warmup", func(c *Config) { c.ChunkBytes++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := cloudflareConfig()
			tc.mutate(&cfg)
			ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("invalid policy caused a network request")
				return nil, nil
			}))
			result := downloadRunner(t).measure(ctx, cfg)
			if result.Error == nil || result.ProbeMethod != cfg.ProbeMethod {
				t.Fatalf("policy result: %+v", result)
			}
		})
	}
}

func TestDownloadTransportIsolationAndRedirectRejection(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	client := newDownloadClient(context.Background())
	defer client.CloseIdleConnections()
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport == http.DefaultTransport || transport.Proxy != nil || !transport.DisableCompression || client.Jar != nil || transport.MaxConnsPerHost != 1 {
		t.Fatal("download transport uses shared state, proxy, compression or cookies")
	}
	var requests atomic.Int32
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		requests.Add(1)
		if req.Header.Get("Cookie") != "" || req.Header.Get("Authorization") != "" || len(req.URL.Query()) != 1 || req.URL.Query().Get("bytes") != "10000000" {
			t.Error("download received authentication state")
		}
		http.Redirect(w, req, "/redirect-target", http.StatusFound)
	}))
	defer ts.Close()
	target, _ := url.Parse(ts.URL)
	ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		local := req.Clone(req.Context())
		local.URL.Scheme, local.URL.Host = target.Scheme, target.Host
		local.Host = ""
		return ts.Client().Transport.RoundTrip(local)
	}))
	result := downloadRunner(t).measure(ctx, cloudflareConfig())
	assertDownloadError(t, result)
	if requests.Load() != 1 {
		t.Fatalf("redirect followed: %d requests", requests.Load())
	}
}

func TestDownloadHTTPBodySampleCutoffCancelsAndClosesStream(t *testing.T) {
	serverDone := make(chan struct{})
	ts := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer close(serverDone)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "10000000")
		if _, err := w.Write(make([]byte, 262144)); err != nil {
			return
		}
		w.(http.Flusher).Flush()
		ticker := time.NewTicker(50 * time.Millisecond)
		defer ticker.Stop()
		body := make([]byte, 32768)
		for {
			select {
			case <-req.Context().Done():
				return
			case <-ticker.C:
				if _, err := w.Write(body); err != nil {
					return
				}
				w.(http.Flusher).Flush()
			}
		}
	}))
	defer ts.Close()
	target, _ := url.Parse(ts.URL)
	ctx := WithDownloadTransportForTesting(context.Background(), downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		local := req.Clone(req.Context())
		local.URL.Scheme, local.URL.Host = target.Scheme, target.Host
		local.Host = ""
		return ts.Client().Transport.RoundTrip(local)
	}))
	cfg := cloudflareConfig()
	cfg.SampleSeconds = 1
	cfg.TimeoutSeconds = 3
	result := downloadRunner(t).measure(ctx, cfg)
	if result.Error != nil || result.BytesReceived < 300000 || result.BytesReceived > 800000 || result.DurationMS < 900 || result.DurationMS > 1500 {
		t.Fatalf("real HTTP sample cancellation became a transport error: %+v", result)
	}
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("HTTP server did not observe stream cancellation")
	}
}

func TestDownloadCancellationSharesAndReleasesProcessSlot(t *testing.T) {
	entered := make(chan struct{})
	body := &downloadBody{stall: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithDownloadTransportForTesting(ctx, downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		body.ctx = req.Context()
		close(entered)
		return downloadResponse(body), nil
	}))
	done := make(chan Report, 1)
	go func() { done <- downloadRunner(t).measure(ctx, cloudflareConfig()) }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("download never started")
	}
	other := NewRunner(testAPI{upload: func(context.Context, string, bool, []byte) (int64, error) {
		t.Error("legacy upload overlapped download")
		return 0, nil
	}}, nil).measure(context.Background(), probeConfig())
	if other.Error == nil {
		t.Fatal("shared slot admitted overlapping legacy probe")
	}
	cancel()
	select {
	case result := <-done:
		assertDownloadError(t, result)
		if !body.closed {
			t.Fatal("cancel did not close body")
		}
	case <-time.After(time.Second):
		t.Fatal("download ignored cancellation")
	}
	next := NewRunner(testAPI{upload: func(_ context.Context, _ string, _ bool, p []byte) (int64, error) { return int64(len(p)), nil }}, nil).measure(context.Background(), probeConfig())
	if next.Error != nil {
		t.Fatalf("slot leaked: %+v", next)
	}
}

func TestRunnerNewMethodUnknownRetainsAuthoritativeBlock(t *testing.T) {
	cfg := cloudflareConfig()
	cfg.State = State{Suspended: true, BlockService: true}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx = WithDownloadTransportForTesting(ctx, downloadRoundTripper(func(req *http.Request) (*http.Response, error) {
		r := downloadResponse(&downloadBody{ctx: req.Context()})
		r.StatusCode = 403
		return r, nil
	}))
	states := make(chan State, 8)
	var reports atomic.Int32
	api := testAPI{
		config: func(context.Context) (Config, error) { return cfg, nil },
		upload: func(context.Context, string, bool, []byte) (int64, error) {
			t.Error("download fell back to upload")
			return 0, nil
		},
		report: func(_ context.Context, result Report) (State, error) {
			assertDownloadError(t, result)
			reports.Add(1)
			return cfg.State, nil
		},
	}
	done := make(chan struct{})
	go func() { fastRunner(api, func(s State) { states <- s }).Run(ctx); close(done) }()
	for range 3 {
		select {
		case state := <-states:
			if !state.BlockService || !state.Suspended {
				t.Errorf("unknown cleared block: %+v", state)
			}
		case <-time.After(time.Second):
			t.Fatal("runner policy polling stalled")
		}
	}
	cancel()
	<-done
	if reports.Load() != 1 {
		t.Fatalf("duplicate lease ran again: %d", reports.Load())
	}
}
