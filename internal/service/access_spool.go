package service

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"github.com/micah123321/mi-node/internal/controlplane"
)

const maxAccessSpoolBytes = 32 << 20

type accessSpool struct {
	Version        int                         `json:"version"`
	Identity       string                      `json:"identity"`
	Reports        []controlplane.AccessReport `json:"reports"`
	DroppedEntries int64                       `json:"dropped_entries"`
	Checksum       string                      `json:"checksum"`
}

func (s *Service) accessSpoolLocation() (string, string, error) {
	path, identity, err := s.spoolLocation()
	if err != nil || path == "" {
		return "", identity, err
	}
	return filepath.Join(filepath.Dir(path), "access-"+identity+".json"), identity, nil
}

func accessChecksum(v accessSpool) string {
	v.Checksum = ""
	data, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *Service) loadAccessSpoolLocked() error {
	if s.accessSpoolLoaded {
		return nil
	}
	path, identity, err := s.accessSpoolLocation()
	if err != nil {
		return err
	}
	if path == "" {
		s.accessSpoolLoaded = true
		return nil
	}
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		s.accessSpoolLoaded = true
		return nil
	}
	if err != nil {
		return err
	}
	if info.Size() > maxAccessSpoolBytes {
		return fmt.Errorf("access spool exceeds size limit (preserved)")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var v accessSpool
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("invalid access spool (preserved): %w", err)
	}
	if v.Version != 1 || v.Identity != identity || v.Checksum != accessChecksum(v) || v.DroppedEntries < 0 || len(v.Reports) > 512 {
		return fmt.Errorf("access spool identity/checksum mismatch (preserved)")
	}
	seen := make(map[string]bool)
	for _, report := range v.Reports {
		if seen[report.ReportID] {
			return fmt.Errorf("duplicate access spool ID (preserved)")
		}
		if err := validateAccessReport(report); err != nil {
			return fmt.Errorf("invalid access spool report (preserved): %w", err)
		}
		seen[report.ReportID] = true
	}
	s.accessQueue = v.Reports
	s.accessDropped += v.DroppedEntries
	s.accessSpoolLoaded = true
	return nil
}

func validateAccessReport(report controlplane.AccessReport) error {
	if report.Version != 1 || report.ReportID == "" || len(report.ReportID) > 128 || report.BillingReportID == "" || len(report.BillingReportID) > 128 || len(report.Entries) > maxAccessEntries || report.ObservedAt <= 0 || report.DroppedEntries < 0 {
		return fmt.Errorf("invalid access report envelope")
	}
	if report.Coverage != "complete" && report.Coverage != "partial" {
		return fmt.Errorf("invalid access coverage")
	}
	sums := make(map[int][2]int64)
	for _, e := range report.Entries {
		budget, exists := report.Totals[e.UserID]
		if !exists || e.UserID <= 0 || e.Upload < 0 || e.Download < 0 || e.FirstSeen <= 0 || e.LastSeen < e.FirstSeen || e.BucketAt <= 0 {
			return fmt.Errorf("invalid destination entry")
		}
		sum := sums[e.UserID]
		if e.Upload > budget[0]-sum[0] || e.Download > budget[1]-sum[1] {
			return fmt.Errorf("destination bytes exceed batch total")
		}
		sums[e.UserID] = [2]int64{sum[0] + e.Upload, sum[1] + e.Download}
	}
	for uid, total := range report.Totals {
		if uid <= 0 || total[0] < 0 || total[1] < 0 || sums[uid] != total {
			return fmt.Errorf("destination bytes do not match batch total")
		}
	}
	return nil
}

func (s *Service) persistAccessQueueLocked(queue []controlplane.AccessReport) error {
	path, identity, err := s.accessSpoolLocation()
	if err != nil || path == "" {
		return err
	}
	v := accessSpool{Version: 1, Identity: identity, Reports: queue, DroppedEntries: s.accessDropped}
	v.Checksum = accessChecksum(v)
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if len(data) > maxAccessSpoolBytes {
		return fmt.Errorf("access spool size limit exceeded")
	}
	return writeAccessSpool(path, data)
}

func writeAccessSpool(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".access-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		syncErr := d.Sync()
		closeErr := d.Close()
		if syncErr != nil {
			return syncErr
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
