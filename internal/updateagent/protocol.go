package updateagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const endpoint = "/api/v2/server/update"

var versionRE = regexp.MustCompile("^v(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)\\.(0|[1-9][0-9]*)$")
var hashRE = regexp.MustCompile("^[0-9a-f]{64}$")
var uuidRE = regexp.MustCompile("^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")

type Versions struct {
	Node *string `json:"mi_node"`
	CLI  *string `json:"xbctl"`
}
type Artifact struct {
	Arch      string `json:"arch"`
	Component string `json:"component"`
	URL       string `json:"https_url"`
	SHA       string `json:"sha256"`
	Size      int64  `json:"size_bytes"`
}
type Release struct {
	ID          string     `json:"id"`
	Version     string     `json:"version"`
	OS          string     `json:"os"`
	MinProtocol int        `json:"min_agent_protocol"`
	Artifacts   []Artifact `json:"artifacts"`
	RevokedAt   *string    `json:"revoked_at"`
}
type Candidate struct {
	TaskID    string `json:"task_id"`
	ReleaseID string `json:"release_id"`
	Version   string `json:"version"`
	Revision  int64  `json:"policy_revision"`
}
type Claim struct {
	TaskID       string    `json:"task_id"`
	AttemptID    string    `json:"attempt_id"`
	LeaseToken   string    `json:"lease_token"`
	LeaseExpires time.Time `json:"lease_expires_at"`
	Release      Release   `json:"release"`
}
type Event struct {
	InstallationID string   `json:"installation_id"`
	AttemptID      string   `json:"attempt_id"`
	LeaseToken     string   `json:"lease_token"`
	Seq            int64    `json:"seq"`
	State          string   `json:"state"`
	OccurredAt     string   `json:"occurred_at"`
	Code           *string  `json:"code"`
	Message        *string  `json:"message"`
	Observed       Versions `json:"observed_versions"`
}
type APIError struct {
	Status     int
	Code       string
	RetryAfter time.Duration
}

func (e *APIError) Error() string { return fmt.Sprintf("update API HTTP %d: %s", e.Status, e.Code) }

type Client struct {
	Authority, Token string
	HTTP             *http.Client
}

func validHTTPS(raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("HTTPS URL without userinfo/fragment required")
	}
	return nil
}
func NewClient(authority, token string) (*Client, error) {
	if e := validHTTPS(authority); e != nil {
		return nil, e
	}
	u, _ := url.Parse(authority)
	if u.RawQuery != "" {
		return nil, fmt.Errorf("authority query forbidden")
	}
	return &Client{strings.TrimRight(authority, "/"), token, &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (c *Client) Post(ctx context.Context, path string, in, out any) error {
	b, e := json.Marshal(in)
	if e != nil {
		return e
	}
	req, e := http.NewRequestWithContext(ctx, "POST", c.Authority+endpoint+path, bytes.NewReader(b))
	if e != nil {
		return e
	}
	req.Header.Set("Content-Type", "application/json")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, e := c.HTTP.Do(req)
	if e != nil {
		return fmt.Errorf("update API transport failed")
	}
	defer res.Body.Close()
	var envelope struct {
		Data  json.RawMessage `json:"data"`
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if e = json.NewDecoder(io.LimitReader(res.Body, 2<<20)).Decode(&envelope); e != nil {
		return fmt.Errorf("invalid API response")
	}
	if res.StatusCode != 200 {
		d, _ := strconv.Atoi(res.Header.Get("Retry-After"))
		return &APIError{res.StatusCode, envelope.Error.Code, time.Duration(d) * time.Second}
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Data, out)
}
func CompareVersions(a, b string) (int, error) {
	if !versionRE.MatchString(a) || !versionRE.MatchString(b) {
		return 0, fmt.Errorf("unknown version")
	}
	aa := strings.Split(a[1:], ".")
	bb := strings.Split(b[1:], ".")
	for i := range aa {
		if len(aa[i]) < len(bb[i]) {
			return -1, nil
		}
		if len(aa[i]) > len(bb[i]) {
			return 1, nil
		}
		if aa[i] < bb[i] {
			return -1, nil
		}
		if aa[i] > bb[i] {
			return 1, nil
		}
	}
	return 0, nil
}
func (r Release) Pair(arch string) ([2]Artifact, error) {
	var pair [2]Artifact
	if r.OS != "linux" || r.MinProtocol > 1 || r.MinProtocol < 1 || r.RevokedAt != nil || !versionRE.MatchString(r.Version) {
		return pair, fmt.Errorf("invalid release")
	}
	seen := map[string]bool{}
	counts := map[string]int{}
	for _, a := range r.Artifacts {
		if a.Arch != "amd64" && a.Arch != "arm64" {
			return pair, fmt.Errorf("arch_mismatch")
		}
		i := 0
		if a.Component == "xbctl" {
			i = 1
		} else if a.Component != "mi-node" {
			return pair, fmt.Errorf("invalid component")
		}
		key := a.Arch + "/" + a.Component
		if seen[key] || a.Size <= 0 || a.Size > 512<<20 || !hashRE.MatchString(a.SHA) {
			return pair, fmt.Errorf("invalid artifact")
		}
		seen[key] = true
		counts[a.Arch]++
		if e := validHTTPS(a.URL); e != nil {
			return pair, e
		}
		if a.Arch == arch {
			pair[i] = a
		}
	}
	for _, n := range counts {
		if n != 2 {
			return pair, fmt.Errorf("incomplete artifact pair")
		}
	}
	if pair[0].Component == "" || pair[1].Component == "" {
		return pair, fmt.Errorf("arch_mismatch")
	}
	return pair, nil
}
