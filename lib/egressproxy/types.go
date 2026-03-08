package egressproxy

import "errors"

const (
	DefaultListenPort = 18080
)

var (
	ErrGatewayMismatch = errors.New("egress proxy already initialized with different gateway")
)

// InstanceConfig defines per-instance proxy behavior.
type InstanceConfig struct {
	InstanceID       string
	SourceIP         string
	TAPDevice        string
	MockToRealEnvVar map[string]string // mock literal -> host env var name
}

// GuestConfig is injected into guest config.json when proxy mode is enabled.
type GuestConfig struct {
	Enabled   bool   `json:"enabled"`
	ProxyURL  string `json:"proxy_url"`
	CACertPEM string `json:"ca_cert_pem"`
}
