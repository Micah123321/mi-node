package model

import (
	"github.com/micah123321/mi-node/internal/access"
	"github.com/micah123321/mi-node/internal/config"
)

type NodeSpec struct {
	Protocol        string
	ListenIP        string
	ServerPort      int
	Network         string
	NetworkSettings map[string]any
	Routes          []RouteRule

	KernelType       string
	KernelLogLevel   string
	CustomOutbounds  []OutboundConfig
	CustomRoutes     []map[string]any
	CustomRouteRules []CustomRouteRule
	CertConfig       *config.CertConfig
	AutoTLS          bool
	Domain           string

	Cipher    string
	Plugin    string
	PluginOpt string
	ServerKey string

	TLS         int
	Flow        string
	Decryption  string
	TLSSettings map[string]any

	Host       string
	ServerName string

	Version      int
	UpMbps       int
	DownMbps     int
	Obfs         string
	ObfsPassword string

	CongestionControl string
	PaddingScheme     string
	Transport         string
	TrafficPattern    string

	Multiplex           *MultiplexConfig
	AcceptProxyProtocol bool
	TrafficLimit        *TrafficLimitSpec
	AccessLog           *access.Config
}

type OutboundConfig struct {
	Tag      string
	Protocol string
	Settings map[string]any
	ProxyTag string
}

type RouteRule struct {
	ID          int
	Match       []string
	Action      string
	ActionValue string
}

type MultiplexConfig struct {
	Enabled        bool
	Protocol       string
	MaxConnections int
	MinStreams     int
	MaxStreams     int
	Padding        bool
	Brutal         *BrutalConfig
}

type BrutalConfig struct {
	Enabled  bool
	UpMbps   int
	DownMbps int
}

type TrafficLimitSpec struct {
	Enabled     bool
	Limit       int64
	ResetDay    int
	ResetTime   string
	Timezone    string
	CurrentUsed int64
	LastResetAt int64
	NextResetAt int64
	SuspendedAt int64
	Status      string
}

type UserSpec struct {
	BillingUserID int
	BillingNodeID int
	ID            int
	UUID          string
	SpeedLimit    int
	DeviceLimit   int
}

// AccountID is only for account-wide limits and online state; ID remains the auth identity.
func (u UserSpec) AccountID() int {
	if u.BillingUserID > 0 {
		return u.BillingUserID
	}
	return u.ID
}

// AccountLimits chooses the strictest positive limit if aliases disagree.
func AccountLimits(users []UserSpec) map[int]UserSpec {
	accounts := make(map[int]UserSpec, len(users))
	for _, u := range users {
		id := u.AccountID()
		a := accounts[id]
		a.ID = id
		if u.SpeedLimit > 0 && (a.SpeedLimit <= 0 || u.SpeedLimit < a.SpeedLimit) {
			a.SpeedLimit = u.SpeedLimit
		}
		if u.DeviceLimit > 0 && (a.DeviceLimit <= 0 || u.DeviceLimit < a.DeviceLimit) {
			a.DeviceLimit = u.DeviceLimit
		}
		accounts[id] = a
	}
	return accounts
}

func (n *NodeSpec) GetProxyProtocol() bool {
	if n == nil {
		return false
	}
	if n.AcceptProxyProtocol {
		return true
	}
	if n.NetworkSettings != nil {
		if v, ok := n.NetworkSettings["acceptProxyProtocol"]; ok {
			if b, ok := v.(bool); ok {
				return b
			}
		}
	}
	return false
}

func cloneAnyMap(src map[string]any) map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func cloneMapSlice(src []map[string]any) []map[string]any {
	if len(src) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(src))
	for _, item := range src {
		out = append(out, cloneAnyMap(item))
	}
	return out
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	out := make([]string, len(src))
	copy(out, src)
	return out
}
