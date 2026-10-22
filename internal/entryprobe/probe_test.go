package entryprobe

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ss "github.com/sagernet/sing-shadowsocks"
	"github.com/sagernet/sing-shadowsocks/shadowaead"
	"github.com/sagernet/sing-shadowsocks/shadowaead_2022"
	M "github.com/sagernet/sing/common/metadata"
	N "github.com/sagernet/sing/common/network"
)

func baseConfig() Config {
	return Config{Method: "aes-128-gcm", Password: "test-only-password", Entries: []Entry{{Host: "127.0.0.1", Port: 1}}, TimeoutSeconds: 1}
}

func encodeConfig(t *testing.T, cfg Config) string {
	t.Helper()
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal("cannot encode test configuration")
	}
	return string(data)
}

func TestDecodeLimitsAndValidation(t *testing.T) {
	valid := encodeConfig(t, baseConfig())
	cfg, err := Decode(strings.NewReader(valid))
	if err != nil || cfg.TargetURL != DefaultTargetURL {
		t.Fatal("valid input or default target rejected")
	}
	exact := valid + strings.Repeat(" ", MaxInputBytes-len(valid))
	if _, err := Decode(strings.NewReader(exact)); err != nil {
		t.Fatal("64 KiB input rejected")
	}
	for name, data := range map[string]string{
		"too large": exact + " ", "second object": valid + valid, "trailing garbage": valid + "x",
		"null": "null", "empty": "", "array": "[]", "unknown field": strings.Replace(valid, "{", `{"plugin":true,`, 1),
		"invalid type": strings.Replace(valid, `"timeout_seconds":1`, `"timeout_seconds":"1"`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode(strings.NewReader(data)); err == nil {
				t.Fatal("invalid input accepted")
			}
		})
	}
	if _, err := Decode(failingReader{}); !errors.Is(err, ErrInvalidInput) {
		t.Fatal("reader failure was not sanitized")
	}
	cases := map[string]func(*Config){
		"missing method":     func(c *Config) { c.Method = "" },
		"unsupported method": func(c *Config) { c.Method = "none" },
		"stream method":      func(c *Config) { c.Method = "aes-128-ctr" },
		"missing password":   func(c *Config) { c.Password = "" },
		"missing entries":    func(c *Config) { c.Entries = nil },
		"too many entries":   func(c *Config) { c.Entries = make([]Entry, MaxEntries+1) },
		"missing timeout":    func(c *Config) { c.TimeoutSeconds = 0 },
		"large timeout":      func(c *Config) { c.TimeoutSeconds = 16 },
		"negative timeout":   func(c *Config) { c.TimeoutSeconds = -1 },
		"bad host":           func(c *Config) { c.Entries[0].Host = "user@example.org" },
		"empty host":         func(c *Config) { c.Entries[0].Host = "" },
		"zero port":          func(c *Config) { c.Entries[0].Port = 0 },
		"large port":         func(c *Config) { c.Entries[0].Port = 65536 },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			c := baseConfig()
			change(&c)
			if _, err := Decode(strings.NewReader(encodeConfig(t, c))); !errors.Is(err, ErrInvalidConfig) {
				t.Fatal("invalid configuration accepted")
			}
			if _, err := Probe(context.Background(), c); !errors.Is(err, ErrInvalidConfig) {
				t.Fatal("library bypassed validation")
			}
		})
	}
	for _, raw := range []string{"http://example.org", "https://u:p@example.org", "https://example.org/#", "https://example.org/#fragment", "https:///missing", "https:opaque", "https://example.org:0", "https://example.org:65536", "https://example.org:", "https://example.org:bad", "https://bad host/"} {
		c := baseConfig()
		c.TargetURL = raw
		if _, err := Decode(strings.NewReader(encodeConfig(t, c))); err == nil {
			t.Fatal("invalid URL accepted")
		}
	}
	c := baseConfig()
	c.Entries = make([]Entry, MaxEntries)
	for i := range c.Entries {
		c.Entries[i] = Entry{Host: "::1", Port: 65535}
	}
	c.TimeoutSeconds = 15
	c.TargetURL = "https://[::1]:443/a?b=c"
	if _, err := Decode(strings.NewReader(encodeConfig(t, c))); err != nil {
		t.Fatal("valid boundary configuration rejected")
	}
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("sensitive reader failure") }

func testKey(size int, value byte) string {
	return base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{value}, size))
}

func Test2022PasswordFormats(t *testing.T) {
	for _, method := range shadowaead_2022.List {
		size := 32
		if method == "2022-blake3-aes-128-gcm" {
			size = 16
		}
		for _, password := range []string{"not-base64!", testKey(size-1, 1), ":" + testKey(size, 1), testKey(size, 1) + ":"} {
			c := baseConfig()
			c.Method = method
			c.Password = password
			if _, _, err := prepare(c); !errors.Is(err, ErrInvalidConfig) {
				t.Fatal("invalid 2022 password accepted")
			}
		}
		for _, password := range []string{testKey(size, 1), testKey(size, 1) + ":" + testKey(size, 2)} {
			c := baseConfig()
			c.Method = method
			c.Password = password
			_, _, err := prepare(c)
			wantValid := method != "2022-blake3-chacha20-poly1305" || !strings.Contains(password, ":")
			if (err == nil) != wantValid {
				t.Fatal("incorrect 2022 single or multi-user password handling")
			}
		}
	}
}

func startTCP(t *testing.T, handle func(net.Conn)) Entry {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal("cannot listen locally")
	}
	done := make(chan struct{})
	var connections []net.Conn
	var workers sync.WaitGroup
	go func() {
		defer close(done)
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			connections = append(connections, conn)
			workers.Go(func() {
				defer conn.Close()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				handle(conn)
			})
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		<-done
		for _, conn := range connections {
			_ = conn.Close()
		}
		workers.Wait()
	})
	addr := listener.Addr().(*net.TCPAddr)
	return Entry{Host: "127.0.0.1", Port: addr.Port}
}

type relayHandler struct{}

func (relayHandler) NewConnection(ctx context.Context, conn net.Conn, metadata M.Metadata) error {
	defer conn.Close()
	upstream, err := (&net.Dialer{Timeout: 2 * time.Second}).DialContext(ctx, "tcp", metadata.Destination.String())
	if err != nil {
		return err
	}
	defer upstream.Close()
	done := make(chan struct{})
	go func() { _, _ = io.Copy(upstream, conn); _ = upstream.Close(); close(done) }()
	_, _ = io.Copy(conn, upstream)
	_ = conn.Close()
	_ = upstream.Close()
	<-done
	return nil
}
func (relayHandler) NewPacketConnection(context.Context, N.PacketConn, M.Metadata) error { return nil }
func (relayHandler) NewError(context.Context, error)                                     {}

func startSS(t *testing.T, method, password string) Entry {
	t.Helper()
	var service interface {
		NewConnection(context.Context, net.Conn, M.Metadata) error
	}
	var err error
	if slices.Contains(shadowaead.List, method) {
		service, err = shadowaead.NewService(method, nil, password, 60, relayHandler{})
	} else if parts := strings.Split(password, ":"); len(parts) == 2 {
		var multi *shadowaead_2022.MultiService[int]
		multi, err = shadowaead_2022.NewMultiServiceWithPassword[int](method, parts[0], 60, relayHandler{}, nil)
		if err == nil {
			err = multi.UpdateUsersWithPasswords([]int{1}, []string{parts[1]})
			service = multi
		}
	} else {
		var single ss.Service
		single, err = shadowaead_2022.NewServiceWithPassword(method, password, 60, relayHandler{}, nil)
		service = single
	}
	if err != nil {
		t.Fatal("cannot create test Shadowsocks service")
	}
	return startTCP(t, func(conn net.Conn) { _ = service.NewConnection(context.Background(), conn, M.Metadata{}) })
}

func tlsTarget(t *testing.T, handler http.Handler) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(server.Certificate())
	return server, roots
}

func TestRealEncryptedRoundTrip(t *testing.T) {
	methods := append(slices.Clone(shadowaead.List), shadowaead_2022.List...)
	for _, method := range methods {
		multiOptions := []bool{false}
		if method == "2022-blake3-aes-128-gcm" || method == "2022-blake3-aes-256-gcm" {
			multiOptions = append(multiOptions, true)
		}
		for _, multi := range multiOptions {
			t.Run(method+"/multi="+strconv.FormatBool(multi), func(t *testing.T) {
				t.Parallel()
				var requests atomic.Int32
				server, roots := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					requests.Add(1)
					if r.Method != http.MethodGet || !r.Close || r.URL.Path != "/generate_204" || r.URL.RawQuery != "probe=1" {
						t.Error("incorrect HTTP probe request")
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				password := "test-only-password"
				wrong := "incorrect-test-password"
				if strings.HasPrefix(method, "2022-") {
					size := 32
					if method == "2022-blake3-aes-128-gcm" {
						size = 16
					}
					password = testKey(size, 1)
					wrong = testKey(size, 3)
					if multi {
						password += ":" + testKey(size, 2)
						wrong += ":" + testKey(size, 2)
					}
				}
				entry := startSS(t, method, password)
				cfg := baseConfig()
				cfg.Method = method
				cfg.Password = password
				cfg.Entries = []Entry{entry}
				cfg.TargetURL = server.URL + "/generate_204?probe=1"
				cfg.TimeoutSeconds = 3
				result, err := probe(context.Background(), cfg, roots)
				if err != nil || !result.Results[0].Online || requests.Load() != 1 {
					t.Fatal("real encrypted TLS/204 round trip failed")
				}
				cfg.Password = wrong
				result, err = probe(context.Background(), cfg, roots)
				if err != nil || result.Results[0].Online || requests.Load() != 1 {
					t.Fatal("wrong credentials were accepted")
				}
				if multi {
					cfg.Password = strings.Split(password, ":")[1]
					result, err = probe(context.Background(), cfg, roots)
					if err != nil || result.Results[0].Online {
						t.Fatal("multi-user server accepted missing server key")
					}
				}
			})
		}
	}
}

func TestTLSAndHTTPValidation(t *testing.T) {
	var redirected atomic.Int32
	var slow atomic.Bool
	server, roots := tlsTarget(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			w.WriteHeader(204)
		case "/wrong":
			w.WriteHeader(200)
		case "/redirect":
			w.Header().Set("Location", "/redirected")
			w.WriteHeader(302)
		case "/redirected":
			redirected.Add(1)
			w.WriteHeader(204)
		case "/slow":
			slow.Store(true)
			<-r.Context().Done()
		default:
			conn, rw, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error("cannot hijack test connection")
				return
			}
			defer conn.Close()
			raw := "HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n"
			switch r.URL.Path {
			case "/body":
				raw += "unexpected body"
			case "/large-body":
				raw += strings.Repeat("b", maxBodyBytes+100)
			case "/length":
				raw = "HTTP/1.1 204 No Content\r\nContent-Length: 1\r\n\r\nx"
			case "/chunked":
				raw = "HTTP/1.1 204 No Content\r\nTransfer-Encoding: chunked\r\n\r\n0\r\n\r\n"
			case "/large-header":
				raw = "HTTP/1.1 204 No Content\r\nX-Padding: " + strings.Repeat("x", maxResponseBytes+100) + "\r\n\r\n"
			case "/truncated":
				raw = "HTTP/1.1 204 No Content\r\n"
			}
			_, _ = rw.WriteString(raw)
			_ = rw.Flush()
		}
	}))
	cfg := baseConfig()
	cfg.Entries = []Entry{startSS(t, cfg.Method, cfg.Password)}
	for _, path := range []string{"/ok", "/wrong", "/redirect", "/body", "/large-body", "/length", "/chunked", "/large-header", "/truncated", "/slow"} {
		t.Run(path, func(t *testing.T) {
			c := cfg
			c.TargetURL = server.URL + path
			start := time.Now()
			result, err := probe(context.Background(), c, roots)
			if err != nil || result.Results[0].Online != (path == "/ok") {
				t.Fatal("incorrect HTTP status/body outcome")
			}
			if path == "/slow" && (!slow.Load() || time.Since(start) > 2500*time.Millisecond) {
				t.Fatal("response read deadline was not enforced")
			}
		})
	}
	if redirected.Load() != 0 {
		t.Fatal("redirect was followed")
	}
	cfg.TargetURL = server.URL + "/ok"
	result, err := Probe(context.Background(), cfg)
	if err != nil || result.Results[0].Online {
		t.Fatal("untrusted TLS certificate accepted in production API")
	}
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "socks5://127.0.0.1:1")
	t.Setenv("NO_PROXY", "")
	result, err = probe(context.Background(), cfg, roots)
	if err != nil || !result.Results[0].Online {
		t.Fatal("environment proxy affected direct SS transport")
	}
}

func TestTCPAcceptWithoutForwardingAndDeadline(t *testing.T) {
	var accepted atomic.Int32
	entry := startTCP(t, func(conn net.Conn) { accepted.Add(1); _, _ = io.Copy(io.Discard, conn) })
	cfg := baseConfig()
	cfg.Entries = []Entry{entry}
	start := time.Now()
	result, err := Probe(context.Background(), cfg)
	elapsed := time.Since(start)
	if err != nil || result.Results[0].Online || accepted.Load() != 1 {
		t.Fatal("TCP-only entry incorrectly considered online")
	}
	if elapsed < 800*time.Millisecond || elapsed > 2500*time.Millisecond {
		t.Fatal("per-entry timeout was not enforced")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start = time.Now()
	result, err = Probe(ctx, cfg)
	if err != nil || result.Results[0].Online || time.Since(start) > 500*time.Millisecond {
		t.Fatal("parent cancellation did not close blocked connection")
	}
}

func TestConcurrencyAndOrder(t *testing.T) {
	var active, maximum, accepted atomic.Int32
	entry := startTCP(t, func(conn net.Conn) {
		accepted.Add(1)
		current := active.Add(1)
		defer active.Add(-1)
		for previous := maximum.Load(); current > previous && !maximum.CompareAndSwap(previous, current); previous = maximum.Load() {
		}
		_, _ = io.Copy(io.Discard, conn)
	})
	cfg := baseConfig()
	cfg.Entries = make([]Entry, MaxEntries)
	// Alternate a real black hole with immediately refused connections to force completion reordering.
	closed := startTCP(t, func(net.Conn) {})
	for i := range cfg.Entries {
		cfg.Entries[i] = entry
		if i%3 == 1 {
			cfg.Entries[i] = closed
		}
	}
	result, err := Probe(context.Background(), cfg)
	if err != nil || len(result.Results) != MaxEntries {
		t.Fatal("batch failed")
	}
	for i, r := range result.Results {
		if r.Host != cfg.Entries[i].Host || r.Port != cfg.Entries[i].Port || r.Online {
			t.Fatal("order or offline status incorrect")
		}
	}
	if maximum.Load() > MaxConcurrency || maximum.Load() < 2 || accepted.Load() != 14 {
		t.Fatal("concurrency bound or full batch processing failed")
	}
}
