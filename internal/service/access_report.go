package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/controlplane"
	"github.com/micah123321/mi-node/internal/model"
	"github.com/micah123321/mi-node/internal/nlog"
	"github.com/micah123321/mi-node/internal/panel"
)

const maxAccessEntries = 10000
const maxDeferredAccessBytes = 8 << 20

type accessProvider interface {
	ConfigureAccess(access.Config)
	DrainAccess() access.Snapshot
}

func (s *Service) configureAccess(nc *model.NodeSpec) {
	cfg := access.Config{BucketSeconds: 300, MaxEntries: 2000}
	if nc != nil && nc.AccessLog != nil {
		cfg = *nc.AccessLog
	}
	if cfg.BucketSeconds < 60 || cfg.BucketSeconds > 3600 {
		cfg.BucketSeconds = 300
	}
	if cfg.MaxEntries < 1 || cfg.MaxEntries > access.MaxEntries {
		cfg.MaxEntries = 2000
	}
	if _, ok := s.sink.(controlplane.AccessReporter); !ok {
		cfg.Enabled = false
	}
	if p, ok := s.kernel.(accessProvider); ok {
		p.ConfigureAccess(cfg)
	} else {
		cfg.Enabled = false
	}
	s.accessMu.Lock()
	s.accessConfig = cfg
	if !cfg.Enabled {
		s.accessPending = nil
		s.accessDropped = 0
	}
	s.accessMu.Unlock()
}

func accessKey(e access.Entry) string {
	return fmt.Sprintf("%d|%d|%s|%s|%d|%s|%s", e.UserID, e.BucketAt, e.Domain, e.IP, e.Port, e.Network, e.DomainSource)
}

// Sampling the cumulative billing counters and the live destination counters is
// not atomic. Carry excess destination bytes forward instead of over-attributing
// them to the previous accounting sample; represent missing evidence explicitly.
func (s *Service) prepareAccessReport(billingID string, totals map[int][2]int64) *controlplane.AccessReport {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if !s.accessConfig.Enabled || len(totals) == 0 {
		return nil
	}
	p, ok := s.kernel.(accessProvider)
	if !ok {
		return nil
	}
	snapshot := p.DrainAccess()
	s.accessDropped += snapshot.DroppedEntries
	merged := make(map[string]access.Entry, len(s.accessPending)+len(snapshot.Entries))
	add := func(e access.Entry) {
		if e.UserID <= 0 || e.Upload < 0 || e.Download < 0 {
			s.accessDropped++
			return
		}
		key := accessKey(e)
		if old, exists := merged[key]; exists {
			old.Upload += e.Upload
			old.Download += e.Download
			old.Connections += e.Connections
			if old.FirstSeen == 0 || e.FirstSeen < old.FirstSeen {
				old.FirstSeen = e.FirstSeen
			}
			if e.LastSeen > old.LastSeen {
				old.LastSeen = e.LastSeen
			}
			merged[key] = old
		} else if len(merged) < maxAccessEntries {
			merged[key] = e
		} else {
			s.accessDropped++
		}
	}
	for _, e := range s.accessPending {
		add(e)
	}
	for _, e := range snapshot.Entries {
		add(e)
	}
	keys := make([]string, 0, len(merged))
	for key := range merged {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	now := time.Now().Unix()
	remaining := make(map[int][2]int64, len(totals))
	copiedTotals := make(map[int][2]int64, len(totals))
	for uid, value := range totals {
		remaining[uid] = value
		copiedTotals[uid] = value
	}
	entries := make([]access.Entry, 0, len(merged)+len(totals))
	pending := make([]access.Entry, 0)
	coverage := "complete"
	if s.accessDropped > 0 {
		coverage = "partial"
	}
	for _, key := range keys {
		e := merged[key]
		budget := remaining[e.UserID]
		sent := e
		sent.Upload = min(e.Upload, budget[0])
		sent.Download = min(e.Download, budget[1])
		if sent.Upload > 0 || sent.Download > 0 {
			entries = append(entries, sent)
			budget[0] -= sent.Upload
			budget[1] -= sent.Download
			remaining[e.UserID] = budget
			if sent.Domain == "" && sent.IP == "" {
				coverage = "partial"
			}
			e.Connections = 0
		}
		e.Upload -= sent.Upload
		e.Download -= sent.Download
		if e.Upload > 0 || e.Download > 0 {
			if e.LastSeen >= now-3600 {
				pending = append(pending, e)
			} else {
				s.accessDropped++
				coverage = "partial"
			}
		}
	}
	users := make([]int, 0, len(remaining))
	for uid := range remaining {
		users = append(users, uid)
	}
	sort.Ints(users)
	for _, uid := range users {
		value := remaining[uid]
		if value[0] == 0 && value[1] == 0 {
			continue
		}
		entries = append(entries, access.Entry{UserID: uid, Network: "unknown", DomainSource: "unknown", BucketAt: now / int64(s.accessConfig.BucketSeconds) * int64(s.accessConfig.BucketSeconds), FirstSeen: now, LastSeen: now, Upload: value[0], Download: value[1]})
		coverage = "partial"
	}
	s.accessPending = pending
	if len(entries) > maxAccessEntries {
		// The protocol bounds a request. Never let detail cardinality stop billing.
		nlog.Core().Warn("access report exceeds entry limit; retaining no fabricated attribution", "entries", len(entries))
		s.accessDropped += int64(len(entries))
		return nil
	}
	report := &controlplane.AccessReport{Version: 1, ReportID: "access-" + billingID, BillingReportID: billingID, Kernel: s.kernel.Name(), ObservedAt: now, Coverage: coverage, DroppedEntries: s.accessDropped, Totals: copiedTotals, Entries: entries}
	s.accessDropped = 0
	return report
}

func (s *Service) enqueueAccessReport(report controlplane.AccessReport) error {
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if err := s.loadAccessSpoolLocked(); err != nil {
		return err
	}
	for _, existing := range s.accessQueue {
		if existing.ReportID == report.ReportID {
			return nil
		}
	}
	queue := append(append([]controlplane.AccessReport(nil), s.accessQueue...), report)
	if len(queue) > 512 {
		// Bound disk usage during extended outages. The next report exposes the gap.
		s.accessDropped += int64(len(queue[0].Entries))
		queue = queue[len(queue)-512:]
		nlog.Core().Warn("access spool backlog limit reached; oldest detail batch expired")
	}
	for {
		encoded, err := json.Marshal(queue)
		if err != nil {
			return err
		}
		if len(encoded) < maxAccessSpoolBytes-4096 {
			break
		}
		if len(queue) <= 1 {
			return fmt.Errorf("access batch exceeds spool capacity")
		}
		s.accessDropped += int64(len(queue[0].Entries))
		queue = queue[1:]
		nlog.Core().Warn("access spool byte limit reached; oldest detail batch expired")
	}
	if err := s.persistAccessQueueLocked(queue); err != nil {
		return err
	}
	s.accessQueue = queue
	return nil
}

// These methods belong to the serialized accounting worker. Deferred details
// remain durable even when the independent access spool is corrupt/unwritable.
func (s *Service) deferAccessReport(report controlplane.AccessReport) {
	for _, old := range s.deferredAccess {
		if old.ReportID == report.ReportID {
			return
		}
	}
	s.deferredAccess = append(s.deferredAccess, report)
	for len(s.deferredAccess) > 0 {
		data, err := json.Marshal(s.deferredAccess)
		if err == nil && len(s.deferredAccess) <= 128 && len(data) <= maxDeferredAccessBytes {
			break
		}
		s.accessMu.Lock()
		s.accessDropped += int64(len(s.deferredAccess[0].Entries))
		s.accessMu.Unlock()
		s.deferredAccess = s.deferredAccess[1:]
		nlog.Core().Warn("deferred access capacity reached; oldest details expired")
	}
}

func (s *Service) retryDeferredAccess() {
	for len(s.deferredAccess) > 0 {
		if err := s.enqueueAccessReport(s.deferredAccess[0]); err != nil {
			return
		}
		s.deferredAccess = s.deferredAccess[1:]
	}
}

// Called with reportMu held, which serializes admission with shutdown Wait.
func (s *Service) startAccessWorkerLocked() {
	if _, ok := s.sink.(controlplane.AccessReporter); !ok || !s.accessActive.CompareAndSwap(false, true) {
		return
	}
	if s.accessBackoff.shouldSkip() {
		s.accessActive.Store(false)
		return
	}
	s.reportWG.Add(1)
	go func() {
		defer s.reportWG.Done()
		defer s.accessActive.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.flushAccessReports(ctx); err != nil {
			nlog.Core().Warn("access report failed; retaining detail batch", "error", err)
			s.accessBackoff.onFailure()
		} else {
			s.accessBackoff.onSuccess()
		}
	}()
}

func (s *Service) sendAccessReport(ctx context.Context) error {
	sink, ok := s.sink.(controlplane.AccessReporter)
	if !ok {
		return nil
	}
	s.accessMu.Lock()
	if err := s.loadAccessSpoolLocked(); err != nil {
		s.accessMu.Unlock()
		return err
	}
	if len(s.accessQueue) == 0 {
		s.accessMu.Unlock()
		return nil
	}
	report := s.accessQueue[0]
	s.accessMu.Unlock()
	err := sink.ReportAccess(ctx, report)
	expired := errors.Is(err, panel.ErrAccessReportExpired)
	if err != nil && !expired {
		return err
	}
	s.accessMu.Lock()
	defer s.accessMu.Unlock()
	if expired {
		s.accessDropped += int64(len(report.Entries))
		nlog.Core().Warn("expired access batch skipped; continuing fresh details", "report_id", report.ReportID)
	}
	// Enqueue can append, but only the admitted worker removes the head.
	if len(s.accessQueue) > 0 && s.accessQueue[0].ReportID == report.ReportID {
		if err := s.persistAccessQueueLocked(s.accessQueue[1:]); err != nil {
			return err
		}
		s.accessQueue = s.accessQueue[1:]
	}
	return nil
}

func (s *Service) flushAccessReports(ctx context.Context) error {
	for {
		s.accessMu.Lock()
		if err := s.loadAccessSpoolLocked(); err != nil {
			s.accessMu.Unlock()
			return err
		}
		empty := len(s.accessQueue) == 0
		s.accessMu.Unlock()
		if empty {
			return nil
		}
		if err := s.sendAccessReport(ctx); err != nil {
			return err
		}
	}
}
