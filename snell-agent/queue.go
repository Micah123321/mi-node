package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Metrics struct {
	KernelStatus bool `json:"kernel_status"`
	Users        int  `json:"users"`
	Connections  int  `json:"connections"`
	UDP          bool `json:"udp"`
	QUIC         bool `json:"quic"`
}
type Report struct {
	ID      string              `json:"report_id"`
	Traffic map[string][2]int64 `json:"traffic"`
	Alive   map[string][]string `json:"alive"`
	Online  map[string]int      `json:"online"`
	Metrics Metrics             `json:"metrics"`
}
type diskState struct {
	Version  int      `json:"version"`
	NodeID   int64    `json:"node_id"`
	PanelURL string   `json:"panel_url"`
	Pending  []Report `json:"pending"`
}
type reportQueue struct {
	mu    sync.Mutex
	path  string
	state diskState
	lock  *os.File
}

func openQueue(c Config) (*reportQueue, error) {
	if err := os.MkdirAll(c.StateDir, 0700); err != nil {
		return nil, errors.New("create state directory failed")
	}
	lock, err := lockState(filepath.Join(c.StateDir, "agent.lock"))
	if err != nil {
		return nil, err
	}
	q := &reportQueue{path: filepath.Join(c.StateDir, "reports.json"), state: diskState{Version: 1, NodeID: c.NodeID, PanelURL: c.PanelURL}, lock: lock}
	b, err := os.ReadFile(q.path)
	if os.IsNotExist(err) {
		return q, nil
	}
	if err != nil {
		q.close()
		return nil, errors.New("read report queue failed")
	}
	var s diskState
	if decodeStrict(b, &s) != nil || s.Version != 1 || s.NodeID != c.NodeID || s.PanelURL != c.PanelURL {
		q.close()
		return nil, errors.New("invalid report queue or mismatched panel/node identity")
	}
	seen := map[string]bool{}
	for _, r := range s.Pending {
		if len(r.ID) != 32 || seen[r.ID] || r.Traffic == nil || r.Alive == nil || r.Online == nil {
			q.close()
			return nil, errors.New("invalid queued report")
		}
		if _, err := hex.DecodeString(r.ID); err != nil {
			q.close()
			return nil, errors.New("invalid queued report ID")
		}
		for _, v := range r.Traffic {
			if v[0] < 0 || v[1] < 0 {
				q.close()
				return nil, errors.New("negative queued traffic")
			}
		}
		seen[r.ID] = true
	}
	q.state = s
	return q, nil
}
func (q *reportQueue) close() error {
	if q.lock != nil {
		return q.lock.Close()
	}
	return nil
}
func (q *reportQueue) save(s diskState) error {
	b, err := json.Marshal(s)
	if err != nil {
		return errors.New("encode report queue failed")
	}
	f, err := os.CreateTemp(filepath.Dir(q.path), ".reports-*")
	if err != nil {
		return errors.New("create report checkpoint failed")
	}
	name := f.Name()
	defer os.Remove(name)
	fail := func() error { _ = f.Close(); return errors.New("persist report queue failed") }
	if f.Chmod(0600) != nil {
		return fail()
	}
	if _, err = f.Write(b); err != nil {
		return fail()
	}
	if f.Sync() != nil {
		return fail()
	}
	if f.Close() != nil {
		return errors.New("close report checkpoint failed")
	}
	if os.Rename(name, q.path) != nil {
		return errors.New("replace report queue failed")
	}
	// A successful rename is the commit point; sync its directory on Unix.
	q.state = s
	if err = syncStateDir(filepath.Dir(q.path)); err != nil {
		return err
	}
	return nil
}
func (q *reportQueue) enqueue(r Report) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	next := q.state
	next.Pending = append(append([]Report(nil), q.state.Pending...), r)
	if err := q.save(next); err != nil {
		return err
	}
	q.state = next
	return nil
}
func (q *reportQueue) first() (Report, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.state.Pending) == 0 {
		return Report{}, false
	}
	return q.state.Pending[0], true
}
func (q *reportQueue) ack(id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.state.Pending) == 0 || q.state.Pending[0].ID != id {
		return errors.New("report queue ordering error")
	}
	next := q.state
	next.Pending = append([]Report(nil), q.state.Pending[1:]...)
	if err := q.save(next); err != nil {
		return err
	}
	q.state = next
	return nil
}
func reportID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", errors.New("generate report ID failed")
	}
	return hex.EncodeToString(b[:]), nil
}

type trafficStats struct {
	mu      sync.Mutex
	traffic map[string][2]int64
}

func (s *trafficStats) add(uid string, up, down int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.traffic[uid]
	v[0] += int64(up)
	v[1] += int64(down)
	s.traffic[uid] = v
}

// Hold counters until the exact delta batch is committed to disk.
func (s *trafficStats) checkpoint(q *reportQueue, r Report) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.Traffic = make(map[string][2]int64, len(s.traffic))
	for id, v := range s.traffic {
		r.Traffic[id] = v
	}
	err := q.enqueue(r)
	// Even a directory-sync error after rename has committed this batch.
	q.mu.Lock()
	committed := false
	for _, pending := range q.state.Pending {
		if pending.ID == r.ID {
			committed = true
			break
		}
	}
	q.mu.Unlock()
	if committed {
		clear(s.traffic)
	}
	return err
}
