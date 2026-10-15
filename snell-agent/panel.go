package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type panelClient struct {
	base  *url.URL
	token string
	node  int64
	http  *http.Client
}

func newPanel(c Config) *panelClient {
	u, _ := url.Parse(c.PanelURL)
	return &panelClient{u, c.Token, c.NodeID, &http.Client{Timeout: time.Duration(c.RequestTimeout), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}
func (p *panelClient) request(ctx context.Context, method, endpoint string, body []byte) ([]byte, error) {
	u := *p.base
	u.Path = strings.TrimRight(u.Path, "/") + "/api/v2/server/" + endpoint
	q := u.Query()
	q.Set("token", p.token)
	q.Set("node_id", strconv.FormatInt(p.node, 10))
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create panel request failed")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.http.Do(req)
	// net/http errors contain the full URL (including token); never propagate them.
	if err != nil {
		return nil, errors.New("panel transport failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("panel %s HTTP %d", endpoint, resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, (16<<20)+1))
	if err != nil || len(b) > 16<<20 {
		return nil, errors.New("invalid panel response size")
	}
	return b, nil
}
func (p *panelClient) config(ctx context.Context) (PanelConfig, error) {
	var c PanelConfig
	b, err := p.request(ctx, http.MethodGet, "config", nil)
	if err != nil {
		return c, err
	}
	if decodeStrict(b, &c) != nil {
		return c, errors.New("invalid or unsupported panel config (routing/custom outbound is unsupported)")
	}
	return c, c.validate()
}
func (p *panelClient) users(ctx context.Context) ([]User, error) {
	b, err := p.request(ctx, http.MethodGet, "user", nil)
	if err != nil {
		return nil, err
	}
	return parseUsers(b)
}
func (p *panelClient) report(ctx context.Context, r Report) error {
	b, err := json.Marshal(r)
	if err != nil {
		return errors.New("encode report failed")
	}
	b, err = p.request(ctx, http.MethodPost, "report", b)
	if err != nil {
		return err
	}
	var result struct {
		Data bool `json:"data"`
	}
	if json.Unmarshal(b, &result) != nil || !result.Data {
		return errors.New("panel did not acknowledge report")
	}
	return nil
}
