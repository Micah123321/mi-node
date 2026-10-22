// Package entryprobe verifies Shadowsocks entries with an authenticated HTTPS round trip.
package entryprobe

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	ss "github.com/sagernet/sing-shadowsocks"
	"github.com/sagernet/sing-shadowsocks/shadowaead"
	"github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	M "github.com/sagernet/sing/common/metadata"
)

const (
	DefaultTargetURL = "https://www.gstatic.com/generate_204"
	MaxInputBytes    = 64 * 1024
	MaxEntries       = 21
	MaxConcurrency   = 8
	maxResponseBytes = 32 * 1024
	maxBodyBytes     = 4 * 1024
)

var (
	ErrInvalidInput  = errors.New("invalid input")
	ErrInvalidConfig = errors.New("invalid configuration")
)

type Entry struct {
	Host string `json:"host"`
	Port int    `json:"port"`
}

type Config struct {
	Method         string  `json:"method"`
	Password       string  `json:"password"`
	Entries        []Entry `json:"entries"`
	TargetURL      string  `json:"target_url"`
	TimeoutSeconds int     `json:"timeout_seconds"`
}

type Result struct {
	Host   string `json:"host"`
	Port   int    `json:"port"`
	Online bool   `json:"online"`
}

type Output struct {
	Results []Result `json:"results"`
}

// Decode reads exactly one JSON object, including any trailing whitespace in the limit.
// Errors deliberately never include input or underlying protocol errors.
func Decode(input io.Reader) (Config, error) {
	var cfg Config
	data, err := io.ReadAll(io.LimitReader(input, MaxInputBytes+1))
	if err != nil || len(data) > MaxInputBytes {
		return cfg, ErrInvalidInput
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&cfg) != nil || decoder.Decode(new(any)) != io.EOF {
		return Config{}, ErrInvalidInput
	}
	if _, _, err = prepare(cfg); err != nil {
		return Config{}, err
	}
	if cfg.TargetURL == "" {
		cfg.TargetURL = DefaultTargetURL
	}
	return cfg, nil
}

// Probe preserves input order. Network, TLS, authentication and HTTP failures are offline.
func Probe(ctx context.Context, cfg Config) (Output, error) {
	return probe(ctx, cfg, nil)
}

// roots is private to the package so tests can trust a local CA without exposing a TLS bypass.
func probe(ctx context.Context, cfg Config, roots *x509.CertPool) (Output, error) {
	method, target, err := prepare(cfg)
	if err != nil {
		return Output{}, err
	}
	output := Output{Results: make([]Result, len(cfg.Entries))}
	jobs := make(chan int)
	var workers sync.WaitGroup
	for range min(MaxConcurrency, len(cfg.Entries)) {
		workers.Go(func() {
			for i := range jobs {
				entry := cfg.Entries[i]
				entryCtx, cancel := context.WithTimeout(ctx, time.Duration(cfg.TimeoutSeconds)*time.Second)
				online := probeEntry(entryCtx, method, entry, target, roots)
				cancel()
				output.Results[i] = Result{Host: entry.Host, Port: entry.Port, Online: online}
			}
		})
	}
	for i := range cfg.Entries {
		jobs <- i
	}
	close(jobs)
	workers.Wait()
	return output, nil
}

func prepare(cfg Config) (ss.Method, *url.URL, error) {
	if cfg.Password == "" || len(cfg.Entries) == 0 || len(cfg.Entries) > MaxEntries || cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 15 {
		return nil, nil, ErrInvalidConfig
	}
	for _, entry := range cfg.Entries {
		if !validHost(entry.Host) || entry.Port < 1 || entry.Port > 65535 {
			return nil, nil, ErrInvalidConfig
		}
	}
	rawURL := cfg.TargetURL
	if rawURL == "" {
		rawURL = DefaultTargetURL
	}
	target, err := url.Parse(rawURL)
	if err != nil || target.Scheme != "https" || target.Opaque != "" || target.User != nil || strings.Contains(rawURL, "#") || !validHost(target.Hostname()) || strings.HasSuffix(target.Host, ":") {
		return nil, nil, ErrInvalidConfig
	}
	if port := target.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, nil, ErrInvalidConfig
		}
	}
	var method ss.Method
	switch {
	case slices.Contains(shadowaead.List, cfg.Method):
		method, err = shadowaead.New(cfg.Method, nil, cfg.Password)
	case slices.Contains(shadowaead_2022.List, cfg.Method):
		// The library must parse the entire server:user chain and construct identity headers.
		method, err = shadowaead_2022.NewWithPassword(cfg.Method, cfg.Password, nil)
	default:
		return nil, nil, ErrInvalidConfig
	}
	if err != nil {
		return nil, nil, ErrInvalidConfig
	}
	return method, target, nil
}

func validHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		return ip.Zone() == ""
	}
	for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func probeEntry(ctx context.Context, method ss.Method, entry Entry, target *url.URL, roots *x509.CertPool) bool {
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(entry.Host, strconv.Itoa(entry.Port)))
	if err != nil {
		return false
	}
	defer raw.Close()
	stop := context.AfterFunc(ctx, func() { _ = raw.Close() })
	defer stop()
	if deadline, ok := ctx.Deadline(); ok {
		if raw.SetDeadline(deadline) != nil {
			return false
		}
	}
	port := target.Port()
	if port == "" {
		port = "443"
	}
	tunnel, err := method.DialConn(raw, M.ParseSocksaddr(net.JoinHostPort(target.Hostname(), port)))
	if err != nil {
		return false
	}
	secured := tls.Client(tunnel, &tls.Config{
		ServerName: target.Hostname(), RootCAs: roots, MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
	})
	if secured.HandshakeContext(ctx) != nil {
		return false
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}
	request.Close = true
	if request.Write(secured) != nil {
		return false
	}
	// A direct HTTP/1.1 exchange neither inherits proxy settings nor follows redirects.
	limited := &io.LimitedReader{R: secured, N: maxResponseBytes + 1}
	reader := bufio.NewReader(limited)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || len(response.TransferEncoding) != 0 {
		return false
	}
	if length := response.Header.Get("Content-Length"); length != "" && length != "0" {
		return false
	}
	// net/http treats 204 bodies as empty even if bytes follow the headers. Read the
	// wire remainder too; require EOF on our Connection: close request to prove emptiness.
	n, err := io.Copy(io.Discard, io.LimitReader(reader, maxBodyBytes+1))
	return err == nil && n == 0 && limited.N > 0 && ctx.Err() == nil
}
