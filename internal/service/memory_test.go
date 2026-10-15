package service

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/panel"
)

type memoryTestSink struct {
	controlplane.Sink
	err error
}

func (s *memoryTestSink) Report(context.Context, controlplane.ReportPayload) error      { return s.err }
func (s *memoryTestSink) ReportAccess(context.Context, controlplane.AccessReport) error { return s.err }

func memoryAccessReport(id string) controlplane.AccessReport {
	return controlplane.AccessReport{
		Version: 1, ReportID: id, BillingReportID: id, Kernel: "sing-box", Coverage: "complete",
		Totals: map[int][2]int64{1: {10, 20}}, Entries: []access.Entry{destinationEntry(1, 10, 20)},
	}
}

func TestBillingQueueReleasesAcknowledgedPayload(t *testing.T) {
	sink := &memoryTestSink{err: errors.New("offline")}
	s := reportService(sink)
	payload := &controlplane.ReportPayload{ReportID: "billing", Traffic: map[int][2]int64{1: {10, 20}}}
	s.reportQueue = []*controlplane.ReportPayload{payload}
	s.pendingReport = payload
	backing := s.reportQueue
	if err := s.sendReport(context.Background()); err == nil || backing[0] != payload {
		t.Fatal("failed report must retain the same payload for retry")
	}
	sink.err = nil
	if err := s.sendReport(context.Background()); err != nil {
		t.Fatal(err)
	}
	if backing[0] != nil || s.reportQueue != nil || s.pendingReport != nil {
		t.Fatal("acknowledged billing payload is still referenced by the queue")
	}
}

func TestAccessQueueReleasesOnlyAcknowledgedOrExpiredPayload(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(fmt.Sprint(expired), func(t *testing.T) {
			sink := &memoryTestSink{err: errors.New("offline")}
			s := reportService(sink)
			s.accessQueue = []controlplane.AccessReport{memoryAccessReport("first"), memoryAccessReport("second")}
			backing := s.accessQueue
			if err := s.sendAccessReport(context.Background()); err == nil || len(backing[0].Entries) != 1 {
				t.Fatal("failed detail report must retain its entries")
			}
			sink.err = nil
			if expired {
				sink.err = panel.ErrAccessReportExpired
			}
			if err := s.sendAccessReport(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(backing[0], controlplane.AccessReport{}) || s.accessQueue[0].ReportID != "second" {
				t.Fatal("sent head retained data or remaining report was modified")
			}
			if err := s.sendAccessReport(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(backing[1], controlplane.AccessReport{}) || s.accessQueue != nil {
				t.Fatal("drained access queue retains its backing payloads")
			}
		})
	}
}

func TestDeferredAccessRecoveryReleasesStagingReferences(t *testing.T) {
	s := reportService(nil)
	s.deferredAccess = []controlplane.AccessReport{memoryAccessReport("first"), memoryAccessReport("second")}
	backing := s.deferredAccess
	s.retryDeferredAccess()
	if s.deferredAccess != nil || len(s.accessQueue) != 2 {
		t.Fatal("recovered staging queue not released")
	}
	for _, report := range backing {
		if !reflect.DeepEqual(report, controlplane.AccessReport{}) {
			t.Fatal("staging queue retains recovered entries")
		}
	}
	if len(s.accessQueue[0].Entries) != 1 || len(s.accessQueue[1].Entries) != 1 {
		t.Fatal("recovering cleared the destination's owned entries")
	}
}

func TestDeferredAccessEvictionReleasesExpiredReferences(t *testing.T) {
	s := reportService(nil)
	s.deferredAccess = make([]controlplane.AccessReport, 128, 129)
	for i := range s.deferredAccess {
		s.deferredAccess[i] = memoryAccessReport(fmt.Sprint(i))
	}
	backing := s.deferredAccess
	s.deferAccessReport(memoryAccessReport("new"))
	if len(s.deferredAccess) != 128 || s.accessDropped != 1 {
		t.Fatal("deferred capacity contract changed")
	}
	if !reflect.DeepEqual(backing[0], controlplane.AccessReport{}) {
		t.Fatal("evicted detail batch remains referenced")
	}
}
