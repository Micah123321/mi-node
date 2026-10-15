package agent

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

const meterChunk = 16 * 1024

// A shared, paced byte budget per user and direction (1 Mbps = 125000 B/s).
type pacer struct {
	rate atomic.Int64
	once sync.Once
	turn chan struct{}
}

func (p *pacer) set(mbps int64) { p.rate.Store(mbps) }
func (p *pacer) wait(ctx context.Context, n int) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.rate.Load() == 0 || n == 0 {
		return nil
	}
	p.once.Do(func() { p.turn = make(chan struct{}, 1) })
	// Cancelled streams leave no future reservations behind for other streams.
	select {
	case <-ctx.Done():
		return ctx.Err()
	case p.turn <- struct{}{}:
	}
	defer func() { <-p.turn }()
	rate := p.rate.Load()
	if rate == 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(time.Duration(int64(n) * int64(time.Second) / (rate * 125000)))
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type meteredConn struct {
	net.Conn
	agent     *Agent
	transport *transportConn
	user      *userState
	uid       string
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	closed    bool
	ops       sync.WaitGroup
	once      sync.Once
}

func (m *meteredConn) begin() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return false
	}
	m.ops.Add(1)
	return true
}
func (m *meteredConn) Read(p []byte) (int, error) {
	if !m.begin() {
		return 0, net.ErrClosed
	}
	defer m.ops.Done()
	if len(p) > meterChunk {
		p = p[:meterChunk]
	}
	n, err := m.Conn.Read(p)
	if n > 0 {
		// Bytes read from the destination are downloads, even if the client closes meanwhile.
		m.agent.stats.add(m.uid, 0, n)
		if e := m.user.down.wait(m.ctx, n); e != nil {
			return n, e
		}
	}
	return n, err
}
func (m *meteredConn) Write(p []byte) (int, error) {
	if !m.begin() {
		return 0, net.ErrClosed
	}
	defer m.ops.Done()
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > meterChunk {
			n = meterChunk
		}
		if err := m.user.up.wait(m.ctx, n); err != nil {
			return total, err
		}
		wrote, err := m.Conn.Write(p[:n])
		if wrote > 0 {
			m.agent.stats.add(m.uid, wrote, 0)
			total += wrote
			p = p[wrote:]
		}
		if err != nil {
			return total, err
		}
		if wrote == 0 {
			return total, net.ErrClosed
		}
	}
	return total, nil
}

// Relay uses an immediate read deadline when either direction finishes. Cancel
// pacing too, so a reused session can finish without waiting behind the same user's other streams.
func (m *meteredConn) SetReadDeadline(t time.Time) error {
	if !t.IsZero() && !t.After(time.Now()) {
		m.cancel()
	}
	return m.Conn.SetReadDeadline(t)
}
func (m *meteredConn) Close() error {
	m.once.Do(func() {
		m.mu.Lock()
		m.closed = true
		m.mu.Unlock()
		m.cancel()
		_ = m.Conn.Close()
		m.ops.Wait()
		m.agent.mu.Lock()
		delete(m.transport.upstreams, m)
		m.agent.mu.Unlock()
	})
	return nil
}
