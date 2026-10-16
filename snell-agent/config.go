package agent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"time"
	"unicode/utf8"
)

type Duration time.Duration

func (d *Duration) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) != nil {
		return errors.New("duration must be a string such as 30s")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return errors.New("invalid duration")
	}
	*d = Duration(v)
	return nil
}

type Config struct {
	PanelURL       string   `json:"panel_url"`
	Token          string   `json:"token"`
	NodeID         int64    `json:"node_id"`
	PullInterval   Duration `json:"pull_interval"`
	ReportInterval Duration `json:"report_interval"`
	StateDir       string   `json:"state_dir"`
	StaleAfter     Duration `json:"stale_after"`
	ListenIP       string   `json:"listen_ip,omitempty"`
	RequestTimeout Duration `json:"request_timeout"`
}

func LoadConfig(path string) (Config, error) {
	var c Config
	f, err := os.Open(path)
	if err != nil {
		return c, errors.New("open agent config failed")
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return c, errors.New("read agent config failed")
	}
	if err = decodeStrict(b, &c); err != nil {
		return c, errors.New("invalid agent config or unsupported field")
	}
	return c, c.normalize()
}
func (c *Config) normalize() error {
	u, err := url.Parse(c.PanelURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("invalid panel_url")
	}
	if c.Token == "" || c.NodeID <= 0 {
		return errors.New("token and positive node_id are required")
	}
	if c.StateDir == "" {
		c.StateDir = "./state"
	}
	if c.StaleAfter == 0 {
		c.StaleAfter = Duration(5 * time.Minute)
	}
	if c.RequestTimeout == 0 {
		c.RequestTimeout = Duration(10 * time.Second)
	}
	if c.StaleAfter < Duration(10*time.Millisecond) || c.RequestTimeout <= 0 || c.PullInterval < 0 || c.ReportInterval < 0 {
		return errors.New("invalid interval or staleness")
	}
	if c.ListenIP != "" && net.ParseIP(c.ListenIP) == nil {
		return errors.New("listen_ip must be an IP literal")
	}
	return nil
}

func decodeStrict(b []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("unexpected trailing JSON")
	}
	return nil
}

type PanelConfig struct {
	Protocol   string `json:"protocol"`
	ListenIP   string `json:"listen_ip"`
	ServerPort int    `json:"server_port"`
	Version    int    `json:"version"`
	Obfs       string `json:"obfs"`
	ObfsHost   string `json:"obfs_host"`
	UDP        bool   `json:"udp"`
	QUIC       bool   `json:"quic,omitempty"`
	BaseConfig struct {
		PullInterval int `json:"pull_interval"`
		PushInterval int `json:"push_interval"`
	} `json:"base_config"`
	TrafficLimit TrafficLimit `json:"traffic_limit"`
}

type TrafficLimit struct {
	Enabled     bool    `json:"enabled"`
	Limit       int64   `json:"limit"`
	CurrentUsed int64   `json:"current_used"`
	Suspended   bool    `json:"suspended"`
	Status      string  `json:"status"`
	ResetDay    int     `json:"reset_day"`
	ResetTime   *string `json:"reset_time"`
	Timezone    *string `json:"timezone"`
	LastResetAt int64   `json:"last_reset_at"`
	NextResetAt int64   `json:"next_reset_at"`
	SuspendedAt int64   `json:"suspended_at"`
}

func (p PanelConfig) suspended() bool {
	return p.TrafficLimit.Suspended || p.TrafficLimit.Status == "suspended" || (p.TrafficLimit.Enabled && p.TrafficLimit.Limit > 0 && p.TrafficLimit.CurrentUsed >= p.TrafficLimit.Limit)
}
func (p PanelConfig) validate() error {
	if p.Protocol != "snell" || p.Version != 4 || p.UDP || p.QUIC {
		return errors.New("only Snell v4 TCP with UDP=false and QUIC=false is supported")
	}
	if p.Obfs != "off" && p.Obfs != "http" {
		return errors.New("only off/http obfs is supported")
	}
	if p.ObfsHost != "" {
		return errors.New("server obfs_host must be empty")
	}
	if p.ServerPort < 1 || p.ServerPort > 65535 || (p.ListenIP != "" && net.ParseIP(p.ListenIP) == nil) {
		return errors.New("invalid panel listener")
	}
	if p.BaseConfig.PullInterval < 0 || p.BaseConfig.PullInterval > 86400 || p.BaseConfig.PushInterval < 0 || p.BaseConfig.PushInterval > 86400 {
		return errors.New("invalid panel interval")
	}
	if p.TrafficLimit.Limit < 0 || p.TrafficLimit.CurrentUsed < 0 {
		return errors.New("invalid traffic limit")
	}
	return nil
}

type User struct {
	ID          int64  `json:"id"`
	UUID        string `json:"uuid"`
	SpeedLimit  int64  `json:"speed_limit"`
	DeviceLimit int    `json:"device_limit"`
}

func parseUsers(b []byte) ([]User, error) {
	if !utf8.Valid(b) {
		return nil, errors.New("invalid user snapshot encoding")
	}
	// Pointer fields distinguish a deliberately empty snapshot from missing data.
	var body struct {
		Users *[]struct {
			ID      *int64  `json:"id"`
			UUID    *string `json:"uuid"`
			Speed   *int64  `json:"speed_limit"`
			Devices *int    `json:"device_limit"`
		} `json:"users"`
	}
	if decodeStrict(b, &body) != nil || body.Users == nil {
		return nil, errors.New("invalid user snapshot")
	}
	out := make([]User, 0, len(*body.Users))
	ids := map[int64]bool{}
	keys := map[string]bool{}
	for _, u := range *body.Users {
		if u.ID == nil || *u.ID <= 0 || u.UUID == nil || *u.UUID == "" || !utf8.ValidString(*u.UUID) || len(*u.UUID) > 4096 || u.Speed == nil || *u.Speed < 0 || *u.Speed > 1_000_000 || u.Devices == nil || *u.Devices < 0 || ids[*u.ID] || keys[*u.UUID] {
			return nil, errors.New("invalid or duplicate user record")
		}
		ids[*u.ID] = true
		keys[*u.UUID] = true
		out = append(out, User{*u.ID, *u.UUID, *u.Speed, *u.Devices})
	}
	return out, nil
}
func (c Config) interval(local Duration, panel int, fallback time.Duration) time.Duration {
	if local > 0 {
		return time.Duration(local)
	}
	if panel > 0 {
		return time.Duration(panel) * time.Second
	}
	return fallback
}
func (c Config) address(p PanelConfig) string {
	ip := c.ListenIP
	if ip == "" {
		ip = p.ListenIP
	}
	if ip == "" {
		ip = "0.0.0.0"
	}
	return net.JoinHostPort(ip, fmt.Sprint(p.ServerPort))
}
