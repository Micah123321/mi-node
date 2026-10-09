package service

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/micah123321/mi-node/internal/controlplane"
)

// Only accounting data is persisted; credentials and transient metrics never enter the spool.
type spoolBatch struct {
	ReportID string           `json:"report_id"`
	Traffic  map[int][2]int64 `json:"traffic"`
}
type reportSpool struct {
	Version  int          `json:"version"`
	Identity string       `json:"identity"`
	Batches  []spoolBatch `json:"batches"`
	Checksum string       `json:"checksum"`
}

func (s *Service) spoolLocation() (string, string, error) {
	if s.cfg == nil {
		return "", "", nil
	}
	if strings.TrimSpace(s.cfg.Kernel.ConfigDir) == "" {
		return "", "", errors.New("report spool requires kernel config_dir")
	}
	identity := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\n%d", strings.TrimRight(s.cfg.Panel.URL, "/"), s.cfg.Panel.NodeID))))
	return filepath.Join(s.cfg.Kernel.ConfigDir, "report-"+identity+".json"), identity, nil
}

func spoolChecksum(v reportSpool) string {
	v.Checksum = ""
	data, _ := json.Marshal(v)
	return fmt.Sprintf("%x", sha256.Sum256(data))
}

func (s *Service) loadReportSpool() error {
	if s.spoolLoaded {
		return nil
	}
	path, identity, err := s.spoolLocation()
	if err != nil {
		return err
	}
	if path == "" {
		s.spoolLoaded = true
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		s.spoolLoaded = true
		return nil
	}
	if err != nil {
		return fmt.Errorf("read report spool: %w", err)
	}
	var v reportSpool
	if err := json.Unmarshal(data, &v); err != nil {
		return fmt.Errorf("decode report spool %s (preserved): %w", path, err)
	}
	if v.Version != 1 || v.Identity != identity || v.Checksum != spoolChecksum(v) {
		return fmt.Errorf("invalid report spool %s: version, identity or checksum mismatch (preserved)", path)
	}
	seen := make(map[string]bool)
	var queue []*controlplane.ReportPayload
	for _, b := range v.Batches {
		if b.ReportID == "" || seen[b.ReportID] || b.Traffic == nil {
			return fmt.Errorf("invalid report spool batch in %s (preserved)", path)
		}
		for uid, traffic := range b.Traffic {
			if uid <= 0 || traffic[0] < 0 || traffic[1] < 0 {
				return fmt.Errorf("invalid report spool traffic in %s (preserved)", path)
			}
		}
		seen[b.ReportID] = true
		queue = append(queue, &controlplane.ReportPayload{ReportID: b.ReportID, Traffic: b.Traffic})
	}
	s.reportQueue = queue
	if len(queue) > 0 {
		s.pendingReport = queue[0]
	}
	s.spoolLoaded = true
	return nil
}

// Atomic replacement also acknowledges batches. A failed update retains the old
// batch and ID, so replay is safe even after an ACK was lost or cleanup failed.
func (s *Service) persistReportQueue(queue []*controlplane.ReportPayload) error {
	path, identity, err := s.spoolLocation()
	if err != nil || path == "" {
		return err
	}
	v := reportSpool{Version: 1, Identity: identity, Batches: make([]spoolBatch, 0, len(queue))}
	for _, p := range queue {
		v.Batches = append(v.Batches, spoolBatch{p.ReportID, p.Traffic})
	}
	v.Checksum = spoolChecksum(v)
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("create report spool directory: %w", err)
	}
	f, err := os.CreateTemp(dir, ".report-*.tmp")
	if err != nil {
		return fmt.Errorf("create report spool temp: %w", err)
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return fmt.Errorf("write report spool: %w", err)
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("replace report spool: %w", err)
	}
	// Windows does not support syncing directory handles. Linux deployments need
	// the directory entry durable as well as the file contents.
	if runtime.GOOS != "windows" {
		d, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = d.Sync()
		closeErr := d.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}
