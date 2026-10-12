package bandwidthcheck

import (
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	CloudflareDownloadV1          = "cloudflare_download_v1"
	CloudflareDownloadURL         = "https://speed.cloudflare.com/__down?bytes=10000000"
	cloudflareDownloadBytes int64 = 10000000
	maxDownloadWarmup             = 262144
)

var errSampleWindow = errors.New("bandwidth sample window elapsed")

type downloadTransportKey struct{}

// WithDownloadTransportForTesting lets local fixtures replace the wire transport.
// The URL, headers, redirect policy and measurement logic still use the real path.
func WithDownloadTransportForTesting(ctx context.Context, transport http.RoundTripper) context.Context {
	return context.WithValue(ctx, downloadTransportKey{}, transport)
}

func newDownloadClient(ctx context.Context) *http.Client {
	var transport http.RoundTripper = &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		MaxIdleConns:       1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1,
		IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
		DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	}
	if injected, ok := ctx.Value(downloadTransportKey{}).(http.RoundTripper); ok && injected != nil {
		transport = injected
	}
	return &http.Client{
		Transport:     transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func measureDownload(ctx context.Context, cfg Config, report Report) Report {
	fail := func(message string) Report { report.Error = &message; return report }
	if cfg.DownloadURL != CloudflareDownloadURL || cfg.MaxBytes != cloudflareDownloadBytes || cfg.ChunkBytes > maxDownloadWarmup {
		return fail("invalid Cloudflare download policy")
	}
	requestCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	client := newDownloadClient(ctx)
	defer client.CloseIdleConnections()
	// Construct from the constant, never from a panel-supplied URL or client.
	req, err := http.NewRequestWithContext(requestCtx, http.MethodGet, CloudflareDownloadURL, nil)
	if err != nil {
		return fail("download: invalid request")
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := client.Do(req)
	if err != nil {
		return fail("download: transport failure or timeout")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Sprintf("download: HTTP %d", resp.StatusCode))
	}
	mediaType, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/octet-stream" {
		return fail("download: invalid content type")
	}
	if resp.Uncompressed {
		return fail("download: encoded response")
	}
	for _, encoding := range resp.Header.Values("Content-Encoding") {
		if encoding = strings.TrimSpace(encoding); encoding != "" && !strings.EqualFold(encoding, "identity") {
			return fail("download: encoded response")
		}
	}
	if resp.ContentLength >= 0 && resp.ContentLength != cloudflareDownloadBytes {
		return fail("download: invalid content length")
	}
	if _, err := io.CopyN(io.Discard, resp.Body, int64(cfg.ChunkBytes)); err != nil {
		return fail("download: incomplete warmup or timeout")
	}

	// Only the continuous body read after warmup contributes to the speed sample.
	started := time.Now()
	window := time.Duration(cfg.SampleSeconds) * time.Second
	deadline := started.Add(window)
	timer := time.AfterFunc(window, func() { cancel(errSampleWindow) })
	defer timer.Stop()
	remaining := cfg.MaxBytes - int64(cfg.ChunkBytes)
	var lastData time.Time
	previousRead := started
	var longestReadGap time.Duration
	recent := min(time.Second, window/2)
	var buf [32 * 1024]byte
	finishWindow := func() Report {
		// A burst followed by a dead connection is unknown, not a low-speed result.
		if ctx.Err() != nil || report.BytesReceived == 0 || lastData.IsZero() || deadline.Sub(lastData) > recent || longestReadGap > recent {
			return fail("download: no recent continuous data at sample cutoff")
		}
		return report
	}
	for report.BytesReceived < remaining {
		if ctx.Err() != nil {
			return fail("download: canceled or timed out")
		}
		if !time.Now().Before(deadline) {
			report.DurationMS = float64(time.Since(started)) / float64(time.Millisecond)
			return finishWindow()
		}
		n, readErr := resp.Body.Read(buf[:int(min(int64(len(buf)), remaining-report.BytesReceived))])
		now := time.Now()
		report.DurationMS = float64(now.Sub(started)) / float64(time.Millisecond)
		if n > 0 {
			report.BytesReceived += int64(n)
			longestReadGap = max(longestReadGap, now.Sub(previousRead))
			previousRead = now
			lastData = now
		}
		if ctx.Err() != nil {
			return fail("download: canceled or timed out")
		}
		if readErr != nil {
			if readErr == io.EOF && report.BytesReceived == remaining {
				break
			}
			if errors.Is(context.Cause(requestCtx), errSampleWindow) &&
				(errors.Is(readErr, context.Canceled) || errors.Is(readErr, errSampleWindow)) {
				return finishWindow()
			}
			return fail("download: incomplete body or read failure")
		}
	}
	if report.BytesReceived == 0 || report.DurationMS <= 0 {
		return fail("download: empty sample")
	}
	return report
}
