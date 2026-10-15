package kernel_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/kernel"
	"github.com/micah123321/mi-node/internal/kernel/singbox"
	"github.com/micah123321/mi-node/internal/kernel/xray"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/nlog"
	"github.com/shirou/gopsutil/v4/process"
	xnet "github.com/xtls/xray-core/common/net"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/uuid"
	"github.com/xtls/xray-core/proxy/vless"
	vlessencoding "github.com/xtls/xray-core/proxy/vless/encoding"
)

const (
	connectionProfileUUID     = "11111111-1111-4111-8111-111111111111"
	connectionProfileUserID   = 123
	connectionProfileTimeout  = 5 * time.Second
	connectionProfilePayload  = "connection-profile-client-upload-and-echo"
	connectionProfileGreeting = "server-only-greeting-for-asymmetric-billing-check\n"
)

type connectionProfileSample struct {
	Stage            string   `json:"stage"`
	Requested        int      `json:"requested_connections"`
	Active           int      `json:"active_connections"`
	AliveIPs         []string `json:"alive_ips"`
	Upload           int64    `json:"upload_bytes"`
	Download         int64    `json:"download_bytes"`
	HeapAlloc        uint64   `json:"heap_alloc_bytes"`
	StackInuse       uint64   `json:"stack_inuse_bytes"`
	HeapIdle         uint64   `json:"heap_idle_bytes"`
	HeapReleased     uint64   `json:"heap_released_bytes"`
	RSS              uint64   `json:"rss_bytes"`
	Goroutines       int      `json:"goroutines"`
	NumGC            uint32   `json:"num_gc"`
	SampledAt        string   `json:"sampled_at_utc"`
	ValidationErrors []string `json:"validation_errors,omitempty"`
	DeferredDownload int64    `json:"deferred_download_bytes,omitempty"`
}

type connectionProfileRun struct {
	Kernel    string                    `json:"kernel"`
	Completed bool                      `json:"completed"`
	PID       int                       `json:"kernel_pid"`
	Samples   []connectionProfileSample `json:"samples"`
}

type connectionProfileReport struct {
	SchemaVersion int                    `json:"schema_version"`
	Scope         string                 `json:"scope"`
	Protocol      string                 `json:"protocol"`
	Measurement   string                 `json:"measurement"`
	GoVersion     string                 `json:"go_version"`
	GOOS          string                 `json:"goos"`
	GOARCH        string                 `json:"goarch"`
	GOMAXPROCS    int                    `json:"gomaxprocs"`
	ParentPID     int                    `json:"load_generator_pid"`
	Counts        []int                  `json:"connection_counts"`
	Runs          []connectionProfileRun `json:"runs"`
}

type connectionProfileRequest struct {
	Op                    string `json:"op"`
	Kernel                string `json:"kernel,omitempty"`
	Protocol              string `json:"protocol,omitempty"`
	ConfigDir             string `json:"config_dir,omitempty"`
	Port                  int    `json:"port,omitempty"`
	TargetPort            int    `json:"target_port,omitempty"`
	Stage                 string `json:"stage,omitempty"`
	Connections           int    `json:"connections"`
	Upload                int64  `json:"upload_bytes"`
	Download              int64  `json:"download_bytes"`
	AllowDeferredDownload bool   `json:"allow_deferred_download,omitempty"`
}

type connectionProfileResponse struct {
	PID    int                      `json:"pid,omitempty"`
	Sample *connectionProfileSample `json:"sample,omitempty"`
	Error  string                   `json:"error,omitempty"`
}

// TestConnectionProfile is an opt-in experiment, not a memory-budget assertion.
// MI_NODE_CONNECTION_PROFILE=1 enables both kernels at 0,100,500 connections.
// MI_NODE_CONNECTION_PROFILE_COUNTS=0,100,500,1000 selects increasing levels;
// MI_NODE_CONNECTION_PROFILE_KERNELS=singbox,xray selects the child backends.
// MI_NODE_CONNECTION_PROFILE_OUTPUT=/path/report.json persists the logged JSON.
// MI_NODE_CONNECTION_PROFILE_PROTOCOL=http selects HTTP CONNECT instead of VLESS.
// HTTP also checks UUID-authenticated billing and connection-close tail bytes.
// Only plain TCP is measured; TLS, QUIC and Mieru are not represented.
func TestConnectionProfile(t *testing.T) {
	if os.Getenv("MI_NODE_CONNECTION_PROFILE") != "1" {
		t.Skip("set MI_NODE_CONNECTION_PROFILE=1 to run the isolated HTTP/TCP experiment")
	}
	counts := connectionProfileCounts(t)
	profileProtocol := os.Getenv("MI_NODE_CONNECTION_PROFILE_PROTOCOL")
	if profileProtocol == "" {
		profileProtocol = "vless"
	}
	if profileProtocol != "vless" && profileProtocol != "http" {
		t.Fatalf("unknown profile protocol %q", profileProtocol)
	}
	backends := "singbox,xray"
	if value := os.Getenv("MI_NODE_CONNECTION_PROFILE_KERNELS"); value != "" {
		backends = value
	}
	report := connectionProfileReport{
		SchemaVersion: 1,
		Scope:         profileProtocol + "/TCP over loopback direct only; excludes TLS, QUIC and Mieru",
		Protocol:      profileProtocol,
		Measurement:   "Child-only MemStats after runtime.GC (test only), OS RSS and goroutines; parent owns clients and echo endpoint. Both backends are linked in the test executable: compare each child's own zero-connection baseline. GC does not force RSS/idle pages back to the OS.",
		GoVersion:     runtime.Version(), GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GOMAXPROCS: runtime.GOMAXPROCS(0), ParentPID: os.Getpid(), Counts: counts,
	}
	defer func() {
		data, err := json.Marshal(report)
		if err != nil {
			t.Error(err)
			return
		}
		t.Logf("connection_profile_json=%s", data)
		if path := os.Getenv("MI_NODE_CONNECTION_PROFILE_OUTPUT"); path != "" {
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				t.Errorf("persist connection profile: %v", err)
			} else {
				t.Logf("connection profile saved to %s", path)
			}
		}
	}()
	seen := make(map[string]bool)
	for _, name := range strings.Split(backends, ",") {
		name = strings.TrimSpace(name)
		if (name != "singbox" && name != "xray") || seen[name] {
			t.Fatalf("invalid or duplicate profile kernel %q", name)
		}
		seen[name] = true
		report.Runs = append(report.Runs, connectionProfileRun{Kernel: name})
		run := &report.Runs[len(report.Runs)-1]
		t.Run(name, func(t *testing.T) {
			endpoint := newConnectionProfileEndpoint(t)
			child := newConnectionProfileChild(t)
			port := connectionProfilePort(t)
			response := child.mustRequest(t, connectionProfileRequest{
				Op: "start", Kernel: name, Protocol: profileProtocol, ConfigDir: t.TempDir(), Port: port, TargetPort: endpoint.listener.Addr().(*net.TCPAddr).Port,
			})
			run.PID = response.PID
			if run.PID <= 0 || run.PID == os.Getpid() {
				t.Fatalf("kernel was not isolated: child=%d parent=%d", run.PID, os.Getpid())
			}
			var clients []*connectionProfileTunnel
			t.Cleanup(func() {
				for _, c := range clients {
					c.Close()
				}
			})
			var upload, download int64
			// Native HTTP splice submits downlink stats when ReadFrom returns.
			deferredDownlink := profileProtocol == "http" && name == "xray" && (runtime.GOOS == "linux" || runtime.GOOS == "android")
			for _, count := range counts {
				oldCount := len(clients)
				for len(clients) < count {
					c, err := dialConnectionProfileTunnel(port, endpoint.listener.Addr().String(), profileProtocol)
					if err != nil {
						t.Fatalf("connect %d/%d: %v", len(clients)+1, count, err)
					}
					clients = append(clients, c)
					upload += int64(len(connectionProfilePayload))
					download += int64(len(connectionProfileGreeting) + len(connectionProfilePayload))
				}
				// Re-probe retained tunnels so each sampled level has live end-to-end I/O.
				for _, c := range clients[:oldCount] {
					if err := c.probe(false); err != nil {
						t.Fatalf("retained tunnel at %d connections: %v", count, err)
					}
					upload += int64(len(connectionProfilePayload))
					download += int64(len(connectionProfilePayload))
				}
				response := child.mustRequest(t, connectionProfileRequest{
					Op: "sample", Stage: "active", Connections: count, Upload: upload, Download: download, AllowDeferredDownload: deferredDownlink,
				})
				connectionProfileCheckSample(t, response.Sample, count, upload, download, deferredDownlink)
				run.Samples = append(run.Samples, *response.Sample)
			}
			for _, c := range clients {
				c.Close()
			}
			clients = nil
			// Sample while the kernel still runs; Stop must not hide stale connection state.
			response = child.mustRequest(t, connectionProfileRequest{
				Op: "sample", Stage: "closed", Connections: 0, Upload: upload, Download: download,
			})
			connectionProfileCheckSample(t, response.Sample, 0, upload, download, false)
			run.Samples = append(run.Samples, *response.Sample)
			// A second GC sample distinguishes sync.Pool victim caches from live flows.
			response = child.mustRequest(t, connectionProfileRequest{Op: "sample", Stage: "reclaimed", Connections: 0, Upload: upload, Download: download})
			connectionProfileCheckSample(t, response.Sample, 0, upload, download, false)
			run.Samples = append(run.Samples, *response.Sample)
			child.mustRequest(t, connectionProfileRequest{Op: "stop"})
			select {
			case <-child.done:
				if child.waitErr != nil {
					t.Fatalf("kernel child exited: %v; stderr: %s", child.waitErr, child.logs())
				}
				run.Completed = !t.Failed()
			case <-time.After(2 * connectionProfileTimeout):
				t.Fatal("kernel child did not exit after Stop")
			}
		})
	}
}

func connectionProfileCounts(t *testing.T) []int {
	t.Helper()
	value := os.Getenv("MI_NODE_CONNECTION_PROFILE_COUNTS")
	if value == "" {
		value = "0,100,500"
	}
	var counts []int
	for _, part := range strings.Split(value, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil || n < 0 || (len(counts) == 0 && n != 0) || (len(counts) > 0 && n <= counts[len(counts)-1]) {
			t.Fatalf("MI_NODE_CONNECTION_PROFILE_COUNTS must start at 0 and strictly increase: %q", value)
		}
		counts = append(counts, n)
	}
	if len(counts) < 2 {
		t.Fatal("connection profile needs a zero baseline and a nonzero connection level")
	}
	return counts
}

func connectionProfileCheckSample(t *testing.T, s *connectionProfileSample, count int, upload, download int64, allowDeferredDownload bool) {
	t.Helper()
	if s == nil {
		t.Fatal("missing child memory sample")
	}
	deferred := allowDeferredDownload && s.Download >= 0 && s.Download < download
	if deferred {
		s.DeferredDownload = download - s.Download
	}
	if s.Active != count || s.Upload != upload || (s.Download != download && !deferred) {
		s.ValidationErrors = append(s.ValidationErrors, fmt.Sprintf("active/billing=%d %d/%d, want %d %d/%d", s.Active, s.Upload, s.Download, count, upload, download))
	}
	if count == 0 && len(s.AliveIPs) != 0 {
		s.ValidationErrors = append(s.ValidationErrors, fmt.Sprintf("closed connections retain alive IPs: %v", s.AliveIPs))
	}
	if count > 0 && (len(s.AliveIPs) != 1 || s.AliveIPs[0] != "127.0.0.1") {
		s.ValidationErrors = append(s.ValidationErrors, fmt.Sprintf("active connections missing loopback identity: %v", s.AliveIPs))
	}
	if s.RSS == 0 || s.Goroutines == 0 {
		s.ValidationErrors = append(s.ValidationErrors, "missing child process metrics")
	}
	for _, err := range s.ValidationErrors {
		t.Errorf("%s/%d: %s", s.Stage, count, err)
	}
}

// The helper exits before the testing harness can append PASS to the JSON stream.
func TestConnectionProfileHelperProcess(t *testing.T) {
	if os.Getenv("MI_NODE_CONNECTION_PROFILE_CHILD") != "1" || os.Getenv("MI_NODE_CONNECTION_PROFILE") != "1" {
		t.Skip("self-exec helper for TestConnectionProfile")
	}
	wire := os.Stdout
	os.Stdout = os.Stderr
	nlog.Init(os.Stderr, slog.LevelError, false)
	enc := json.NewEncoder(wire)
	if err := serveConnectionProfileChild(json.NewDecoder(os.Stdin), enc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Exit(0)
}

func serveConnectionProfileChild(dec *json.Decoder, enc *json.Encoder) (retErr error) {
	var engine kernel.Kernel
	defer func() {
		// Report a failed assertion before Stop spends its bounded drain timeout.
		if retErr != nil {
			_ = enc.Encode(connectionProfileResponse{Error: retErr.Error()})
		}
		if engine != nil {
			engine.Stop()
		}
	}()
	self, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		return err
	}
	// Warm the OS metric reader before measuring the zero-connection baseline.
	ctx, cancel := context.WithTimeout(context.Background(), connectionProfileTimeout)
	_, err = self.MemoryInfoWithContext(ctx)
	cancel()
	if err != nil {
		return fmt.Errorf("warm child RSS: %w", err)
	}
	for {
		var req connectionProfileRequest
		if err := dec.Decode(&req); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		response := connectionProfileResponse{PID: os.Getpid()}
		switch req.Op {
		case "start":
			if engine != nil {
				return fmt.Errorf("kernel already started")
			}
			enableDefaultRules := false
			cfg := config.KernelConfig{
				Type: req.Kernel, ConfigDir: req.ConfigDir, LogLevel: "error",
				Egress: config.EgressConfig{EnableDefaultRules: &enableDefaultRules},
			}
			switch req.Kernel {
			case "singbox":
				engine = singbox.New(cfg)
			case "xray":
				engine = xray.New(cfg)
			default:
				return fmt.Errorf("unknown kernel %q", req.Kernel)
			}
			node := &model.NodeSpec{Protocol: req.Protocol, ListenIP: "127.0.0.1", ServerPort: req.Port}
			// Disabling default rules retains a legacy private-network block. Permit
			// only this fixture's exact TCP endpoint ahead of that legacy rule.
			node.CustomRouteRules = []model.CustomRouteRule{{
				Match:  model.RouteMatch{IPCIDRs: []string{"127.0.0.1/32"}, Networks: []string{"tcp"}, Ports: []string{strconv.Itoa(req.TargetPort)}},
				Action: model.RouteAction{Type: "direct"},
			}}
			users := []model.UserSpec{{ID: connectionProfileUserID, UUID: connectionProfileUUID}}
			if err := engine.Start(node, users, kernel.TLSCert{}); err != nil {
				return fmt.Errorf("start %s: %w", req.Kernel, err)
			}
		case "sample":
			if engine == nil {
				return fmt.Errorf("sample before start")
			}
			response.Sample, err = sampleConnectionProfileChild(engine, self, req)
			if err != nil {
				return err
			}
		case "stop":
			if engine != nil {
				engine.Stop()
				engine = nil
			}
			return enc.Encode(response)
		default:
			return fmt.Errorf("unknown control operation %q", req.Op)
		}
		if err := enc.Encode(response); err != nil {
			return err
		}
	}
}

func sampleConnectionProfileChild(engine kernel.Kernel, self *process.Process, req connectionProfileRequest) (*connectionProfileSample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), connectionProfileTimeout)
	defer cancel()
	s := &connectionProfileSample{Stage: req.Stage, Requested: req.Connections, AliveIPs: []string{}}
	for {
		traffic, alive, count, err := engine.GetUserTraffic(ctx)
		if err != nil {
			return nil, err
		}
		s.Active, s.Upload, s.Download = count, traffic[connectionProfileUserID][0], traffic[connectionProfileUserID][1]
		downloadMatched := s.Download == req.Download || (req.AllowDeferredDownload && s.Download >= 0 && s.Download < req.Download)
		if (count == req.Connections && s.Upload == req.Upload && downloadMatched) || ctx.Err() != nil {
			for ip, live := range alive[connectionProfileUserID] {
				if live {
					s.AliveIPs = append(s.AliveIPs, ip)
				}
			}
			break
		}
		select {
		case <-ctx.Done():
			// Still collect memory on a billing/count mismatch; the parent records
			// the failed assertions in JSON and continues through the closed sample.
		case <-time.After(10 * time.Millisecond):
		}
	}
	runtime.GC()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	rssCtx, rssCancel := context.WithTimeout(context.Background(), connectionProfileTimeout)
	defer rssCancel()
	info, err := self.MemoryInfoWithContext(rssCtx)
	if err != nil {
		return nil, fmt.Errorf("child RSS: %w", err)
	}
	s.HeapAlloc, s.StackInuse = mem.HeapAlloc, mem.StackInuse
	s.HeapIdle, s.HeapReleased = mem.HeapIdle, mem.HeapReleased
	s.RSS, s.Goroutines, s.NumGC = info.RSS, runtime.NumGoroutine(), mem.NumGC
	s.SampledAt = time.Now().UTC().Format(time.RFC3339Nano)
	return s, nil
}

type connectionProfileChild struct {
	cmd     *exec.Cmd
	input   io.WriteCloser
	enc     *json.Encoder
	dec     *json.Decoder
	done    chan struct{}
	waitErr error
	logPath string
}

func newConnectionProfileChild(t *testing.T) *connectionProfileChild {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	logFile, err := os.CreateTemp(t.TempDir(), "kernel-stderr-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logFile.Close() })
	cmd := exec.Command(executable, "-test.run=^TestConnectionProfileHelperProcess$")
	cmd.Env = append(os.Environ(), "MI_NODE_CONNECTION_PROFILE_CHILD=1")
	cmd.Stderr = logFile
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	// Own the pipe explicitly: Cmd.Wait must not close unread final JSON.
	output, outputWriter, err := os.Pipe()
	if err != nil {
		input.Close()
		t.Fatal(err)
	}
	cmd.Stdout = outputWriter
	child := &connectionProfileChild{cmd: cmd, input: input, enc: json.NewEncoder(input), dec: json.NewDecoder(output), done: make(chan struct{}), logPath: logFile.Name()}
	if err := cmd.Start(); err != nil {
		input.Close()
		output.Close()
		outputWriter.Close()
		t.Fatal(err)
	}
	outputWriter.Close()
	go func() {
		child.waitErr = cmd.Wait()
		close(child.done)
	}()
	t.Cleanup(func() {
		input.Close()
		select {
		case <-child.done:
		default:
			_ = cmd.Process.Kill()
			select {
			case <-child.done:
			case <-time.After(connectionProfileTimeout):
				t.Error("timed out reaping kernel child")
			}
		}
		output.Close()
		if t.Failed() {
			t.Logf("kernel child stderr: %s", child.logs())
		}
	})
	return child
}

func (c *connectionProfileChild) logs() string {
	data, _ := os.ReadFile(c.logPath)
	if len(data) > 8192 {
		data = data[len(data)-8192:]
	}
	return string(data)
}

func (c *connectionProfileChild) mustRequest(t *testing.T, req connectionProfileRequest) connectionProfileResponse {
	t.Helper()
	type result struct {
		response connectionProfileResponse
		err      error
	}
	ready := make(chan result, 1)
	go func() {
		var r result
		if r.err = c.enc.Encode(req); r.err == nil {
			r.err = c.dec.Decode(&r.response)
		}
		ready <- r
	}()
	select {
	case r := <-ready:
		if r.err != nil || r.response.Error != "" {
			t.Fatalf("child %s: transport=%v kernel=%s; stderr: %s", req.Op, r.err, r.response.Error, c.logs())
		}
		return r.response
	case <-time.After(2 * connectionProfileTimeout):
		t.Fatalf("child %s timed out after 10s; stderr: %s", req.Op, c.logs())
		return connectionProfileResponse{}
	}
}

func connectionProfilePort(t *testing.T) int {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return port
}

type connectionProfileTunnel struct {
	net.Conn
	reader        *bufio.Reader
	vlessResponse bool
}

func dialConnectionProfileTunnel(port int, target, profileProtocol string) (*connectionProfileTunnel, error) {
	conn, err := net.DialTimeout("tcp4", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), connectionProfileTimeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(time.Now().Add(connectionProfileTimeout))
	reader := bufio.NewReader(conn)
	if profileProtocol == "vless" {
		_, targetPort, splitErr := net.SplitHostPort(target)
		if splitErr != nil {
			conn.Close()
			return nil, splitErr
		}
		remotePort, parseErr := strconv.Atoi(targetPort)
		if parseErr != nil {
			conn.Close()
			return nil, parseErr
		}
		id, parseErr := uuid.ParseString(connectionProfileUUID)
		if parseErr != nil {
			conn.Close()
			return nil, parseErr
		}
		request := &protocol.RequestHeader{Version: 0, Command: protocol.RequestCommandTCP,
			Address: xnet.LocalHostIP, Port: xnet.Port(remotePort),
			User: &protocol.MemoryUser{Account: &vless.MemoryAccount{ID: protocol.NewID(id)}}}
		if err := vlessencoding.EncodeRequestHeader(conn, request, &vlessencoding.Addons{}); err != nil {
			conn.Close()
			return nil, err
		}
	} else {
		auth := base64.StdEncoding.EncodeToString([]byte(connectionProfileUUID + ":" + connectionProfileUUID))
		if _, err := fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Basic %s\r\n\r\n", target, target, auth); err != nil {
			conn.Close()
			return nil, err
		}
		response, err := http.ReadResponse(reader, &http.Request{Method: http.MethodConnect})
		if err != nil {
			conn.Close()
			return nil, err
		}
		if response.StatusCode != http.StatusOK {
			conn.Close()
			return nil, fmt.Errorf("HTTP CONNECT: %s", response.Status)
		}
	}
	tunnel := &connectionProfileTunnel{Conn: conn, reader: reader, vlessResponse: profileProtocol == "vless"}
	if err := tunnel.probe(true); err != nil {
		conn.Close()
		return nil, err
	}
	return tunnel, nil
}

func (c *connectionProfileTunnel) probe(greeting bool) error {
	if err := c.SetDeadline(time.Now().Add(connectionProfileTimeout)); err != nil {
		return err
	}
	if _, err := io.WriteString(c.Conn, connectionProfilePayload); err != nil {
		return err
	}
	if c.vlessResponse {
		if _, err := vlessencoding.DecodeResponseHeader(c.reader, &protocol.RequestHeader{Version: 0}); err != nil {
			return err
		}
		c.vlessResponse = false
	}
	want := connectionProfilePayload
	if greeting {
		want = connectionProfileGreeting + want
	}
	got := make([]byte, len(want))
	if _, err := io.ReadFull(c.reader, got); err != nil {
		return err
	}
	if !bytes.Equal(got, []byte(want)) {
		return fmt.Errorf("TCP echo mismatch: got %q, want %q", got, want)
	}
	return c.SetDeadline(time.Time{})
}

type connectionProfileEndpoint struct {
	listener net.Listener
	mu       sync.Mutex
	closed   bool
	conns    map[net.Conn]struct{}
	wg       sync.WaitGroup
}

func newConnectionProfileEndpoint(t *testing.T) *connectionProfileEndpoint {
	t.Helper()
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	e := &connectionProfileEndpoint{listener: listener, conns: make(map[net.Conn]struct{})}
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			e.mu.Lock()
			if e.closed {
				e.mu.Unlock()
				conn.Close()
				return
			}
			e.conns[conn] = struct{}{}
			e.wg.Add(1)
			e.mu.Unlock()
			go func() {
				defer e.wg.Done()
				defer func() {
					conn.Close()
					e.mu.Lock()
					delete(e.conns, conn)
					e.mu.Unlock()
				}()
				conn.SetWriteDeadline(time.Now().Add(connectionProfileTimeout))
				if _, err := io.WriteString(conn, connectionProfileGreeting); err != nil {
					return
				}
				conn.SetWriteDeadline(time.Time{})
				_, _ = io.Copy(conn, conn)
			}()
		}
	}()
	t.Cleanup(func() {
		e.mu.Lock()
		e.closed = true
		listener.Close()
		for c := range e.conns {
			c.Close()
		}
		e.mu.Unlock()
		done := make(chan struct{})
		go func() { e.wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(connectionProfileTimeout):
			t.Error("timed out cleaning up parent TCP echo endpoint")
		}
	})
	return e
}
