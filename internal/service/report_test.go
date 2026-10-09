package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/micah123321/mi-node/internal/config"
	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/limiter"
	"github.com/micah123321/mi-node/internal/tracker"
)

func reportService(sink controlplane.Sink) *Service {
	l := limiter.New()
	return &Service{sink: sink, source: controlplane.NewLocalControlPlane(&config.Config{}), kernel: &fakeDebugKernel{running: true}, tracker: tracker.New(), limiter: l, speedTracker: limiter.NewSpeedTracker(l)}
}

func TestReportLostResponseRetriesSameBatchWithoutMerging(t *testing.T) {
	var mu sync.Mutex
	var batches []struct {
		ReportID string           `json:"report_id"`
		Traffic  map[int][2]int64 `json:"traffic"`
	}
	seen := make(map[string]bool)
	var charged int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var batch struct {
			ReportID string           `json:"report_id"`
			Traffic  map[int][2]int64 `json:"traffic"`
		}
		if err := json.NewDecoder(r.Body).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		mu.Lock()
		batches = append(batches, batch)
		if !seen[batch.ReportID] {
			charged += batch.Traffic[1][0]
			seen[batch.ReportID] = true
		}
		first := len(batches) == 1
		mu.Unlock()
		if first {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			conn.Close()
			return
		}
		w.Write([]byte(`{"data":true}`))
	}))
	defer server.Close()
	cp := controlplane.NewPanelControlPlane(config.PanelConfig{URL: server.URL, NodeID: 1}, config.WSConfig{}, config.KernelConfig{})
	s := reportService(cp)
	s.tracker.Process(map[int][2]int64{1: {100, 20}}, nil, 0)
	if err := s.sendReport(context.Background()); err == nil {
		t.Fatal("lost response must fail")
	}
	s.tracker.Process(map[int][2]int64{1: {175, 30}}, nil, 0)
	if err := s.sendReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.sendReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(batches) != 3 {
		t.Fatalf("batches=%d", len(batches))
	}
	if batches[0].ReportID == "" || !reflect.DeepEqual(batches[0], batches[1]) {
		t.Fatalf("retry changed batch: %+v", batches)
	}
	if batches[2].ReportID == batches[0].ReportID || batches[2].Traffic[1] != [2]int64{75, 10} {
		t.Fatalf("new traffic merged into retry: %+v", batches)
	}
	if charged != 175 {
		t.Fatalf("charged=%d, want 175", charged)
	}
}

type blockingReportSink struct {
	controlplane.LocalControlPlane
	entered   chan struct{}
	release   chan struct{}
	calls     []controlplane.ReportPayload
	failFinal bool
}

func (s *blockingReportSink) SupportsReporting() bool { return true }
func (s *blockingReportSink) Report(ctx context.Context, p controlplane.ReportPayload) error {
	s.calls = append(s.calls, p)
	if len(s.calls) == 1 {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return errors.New("response lost")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.failFinal {
		return errors.New("panel unavailable")
	}
	return nil
}

type finalTrafficKernel struct {
	fakeDebugKernel
	tail             map[int][2]int64
	sampledAfterStop bool
}

func (k *finalTrafficKernel) GetUserTraffic(ctx context.Context) (map[int][2]int64, map[int]map[string]bool, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, 0, err
	}
	k.sampledAfterStop = !k.running
	return k.tail, nil, 0, nil
}

func TestShutdownWaitsForFailedInflightAndSamplesTail(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "retry-success", true: "final-failure"}[fail], func(t *testing.T) {
			sink := &blockingReportSink{entered: make(chan struct{}), release: make(chan struct{}), failFinal: fail}
			s := reportService(sink)
			k := &finalTrafficKernel{fakeDebugKernel: fakeDebugKernel{running: true}, tail: map[int][2]int64{1: {175, 30}}}
			s.kernel = k
			s.tracker.Process(map[int][2]int64{1: {100, 20}}, nil, 0)
			s.pushReportAsync()
			select {
			case <-sink.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("report did not start")
			}
			done := make(chan error, 1)
			go func() { done <- s.shutdownReports() }()
			select {
			case err := <-done:
				t.Fatalf("shutdown passed in-flight report: %v", err)
			case <-time.After(20 * time.Millisecond):
			}
			close(sink.release)
			select {
			case err := <-done:
				if (err != nil) != fail {
					t.Fatalf("shutdown error=%v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("shutdown hung")
			}
			if !k.sampledAfterStop {
				t.Fatal("final sample did not follow stop")
			}
			if len(sink.calls) < 2 || !reflect.DeepEqual(sink.calls[0], sink.calls[1]) {
				t.Fatal("shutdown retry changed batch")
			}
			if fail {
				if s.pendingReport == nil {
					t.Fatal("failed batch discarded")
				}
				if len(s.reportQueue) != 2 {
					t.Fatalf("queued batches=%d", len(s.reportQueue))
				}
				if got := s.reportQueue[1].Traffic[1]; got != [2]int64{75, 10} {
					t.Fatalf("pending tail=%v", got)
				}
			} else {
				if len(sink.calls) != 3 || sink.calls[2].Traffic[1] != [2]int64{75, 10} || sink.calls[2].ReportID == sink.calls[0].ReportID {
					t.Fatalf("final batch=%+v", sink.calls)
				}
			}
			calls := len(sink.calls)
			s.pushReportAsync()
			s.reportWG.Wait()
			if len(sink.calls) != calls {
				t.Fatal("report admitted after shutdown")
			}
		})
	}
}
