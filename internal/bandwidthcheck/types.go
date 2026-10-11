package bandwidthcheck

import (
	"context"
	"errors"
)

var ErrUnsupported = errors.New("bandwidth API is not available")

type State struct {
	Suspended    bool  `json:"suspended"`
	BlockService bool  `json:"block_service"`
	NextCheckAt  int64 `json:"next_check_at"`
}

type Config struct {
	State
	Enabled          bool    `json:"enabled"`
	Interval         int     `json:"interval"`
	RetryInterval    int     `json:"retry_interval"`
	RecoveryInterval int     `json:"recovery_interval"`
	ThresholdMbps    float64 `json:"threshold_mbps"`
	ProbeMethod      string  `json:"probe_method"`
	DownloadURL      string  `json:"download_url"`
	SampleSeconds    int     `json:"sample_seconds"`
	ChunkBytes       int     `json:"chunk_bytes"`
	MaxBytes         int64   `json:"max_bytes"`
	TimeoutSeconds   int     `json:"timeout_seconds"`
	ProbeID          string  `json:"probe_id"`
	PolicyRevision   string  `json:"policy_revision"`
}

type Report struct {
	ProbeID        string  `json:"probe_id"`
	PolicyRevision string  `json:"policy_revision"`
	ProbeMethod    string  `json:"probe_method"`
	BytesReceived  int64   `json:"bytes_received"`
	DurationMS     float64 `json:"duration_ms"`
	Error          *string `json:"error"`
}

// API deliberately contains no heartbeat or kernel operations.
type API interface {
	Config(context.Context) (Config, error)
	Upload(context.Context, string, bool, []byte) (int64, error)
	Report(context.Context, Report) (State, error)
}
