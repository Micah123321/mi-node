package panel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/micah123321/mi-node/internal/bandwidthcheck"
)

// BandwidthClient has its own transport; a slow upload cannot occupy heartbeat
// connections. Authentication is attached only to this configured panel URL.
type BandwidthClient struct {
	baseURL    string
	auth       url.Values
	httpClient *http.Client
}

func (c *Client) NewBandwidthClient() *BandwidthClient {
	auth := c.authQuery()
	if c.machineID == 0 {
		auth.Del("token")
		auth.Set("server_token", c.token)
	} else {
		auth.Set("node_id", strconv.Itoa(c.nodeID))
	}
	return &BandwidthClient{
		baseURL: c.baseURL, auth: auth,
		httpClient: &http.Client{
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
			Transport: &http.Transport{
				DisableCompression: true,
				MaxIdleConns:       1, MaxIdleConnsPerHost: 1, MaxConnsPerHost: 1,
				IdleConnTimeout: 90 * time.Second, TLSHandshakeTimeout: 10 * time.Second,
				DialContext: (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			},
		},
	}
}

func (c *BandwidthClient) Close() { c.httpClient.CloseIdleConnections() }

func (c *BandwidthClient) Config(ctx context.Context) (bandwidthcheck.Config, error) {
	var cfg bandwidthcheck.Config
	err := c.request(ctx, "GET", "config", nil, nil, &cfg)
	return cfg, err
}

func (c *BandwidthClient) Upload(ctx context.Context, probeID string, warmup bool, body []byte) (int64, error) {
	query := url.Values{"probe_id": {probeID}, "warmup": {"0"}}
	if warmup {
		query.Set("warmup", "1")
	}
	var ack struct {
		ProbeID       string `json:"probe_id"`
		BytesReceived int64  `json:"bytes_received"`
	}
	if err := c.request(ctx, "POST", "upload", query, body, &ack); err != nil {
		return 0, err
	}
	if ack.ProbeID != probeID || ack.BytesReceived != int64(len(body)) {
		return 0, errors.New("bandwidth upload: acknowledgement mismatch")
	}
	return ack.BytesReceived, nil
}

func (c *BandwidthClient) Report(ctx context.Context, report bandwidthcheck.Report) (bandwidthcheck.State, error) {
	var state bandwidthcheck.State
	body, err := json.Marshal(report)
	if err != nil {
		return state, errors.New("bandwidth report: invalid result")
	}
	err = c.request(ctx, "POST", "report", nil, body, &state)
	return state, err
}

func (c *BandwidthClient) request(ctx context.Context, method, operation string, extra url.Values, body []byte, result any) error {
	base, err := url.Parse(c.baseURL)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") || base.Host == "" ||
		base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return errors.New("bandwidth: invalid panel URL")
	}
	endpoint, err := url.Parse(c.baseURL + "/api/v2/server/bandwidth/" + operation)
	if err != nil {
		return errors.New("bandwidth: invalid endpoint")
	}
	query := endpoint.Query()
	for key, values := range c.auth {
		query[key] = values
	}
	for key, values := range extra {
		query[key] = values
	}
	endpoint.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return errors.New("bandwidth: invalid request")
	}
	setCapabilityHeader(req.Header)
	// Never replay a raw upload automatically after a connection failure.
	req.GetBody = nil
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("Content-Type", "application/json")
	if operation == "upload" {
		req.Header.Set("Content-Type", "application/octet-stream")
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		// net/http's URL errors contain authentication query values; never log them.
		if ctx.Err() != nil {
			return fmt.Errorf("bandwidth %s: %w", operation, ctx.Err())
		}
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			return fmt.Errorf("bandwidth %s: request timed out", operation)
		}
		return fmt.Errorf("bandwidth %s: transport failure", operation)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return bandwidthcheck.ErrUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("bandwidth %s: HTTP %d", operation, resp.StatusCode)
	}
	if encoding := resp.Header.Get("Content-Encoding"); encoding != "" && encoding != "identity" {
		return fmt.Errorf("bandwidth %s: compressed response", operation)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64*1024)).Decode(&envelope); err != nil ||
		len(envelope.Data) == 0 || bytes.Equal(bytes.TrimSpace(envelope.Data), []byte("null")) {
		return fmt.Errorf("bandwidth %s: invalid response", operation)
	}
	if operation == "config" || operation == "report" {
		var state struct {
			Suspended    *bool  `json:"suspended"`
			BlockService *bool  `json:"block_service"`
			NextCheckAt  *int64 `json:"next_check_at"`
			Enabled      *bool  `json:"enabled"`
		}
		if err := json.Unmarshal(envelope.Data, &state); err != nil || state.Suspended == nil || state.BlockService == nil || state.NextCheckAt == nil || (operation == "config" && state.Enabled == nil) {
			return fmt.Errorf("bandwidth %s: incomplete policy state", operation)
		}
	}
	if err := json.Unmarshal(envelope.Data, result); err != nil {
		return fmt.Errorf("bandwidth %s: invalid response data", operation)
	}
	return nil
}
