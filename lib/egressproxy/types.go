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
	InstanceID         string
	SourceIP           string
	TAPDevice          string
	BlockAllTCPEgress  bool
	SecretRewriteRules []SecretRewriteRuleConfig
}

// SecretRewriteRuleConfig defines one mocked secret substitution policy.
type SecretRewriteRuleConfig struct {
	MockValue      string
	RealValue      string
	AllowedDomains []string // optional exact or *.example.com patterns; empty means allow all
}

// ServiceOptions customizes service construction (primarily for tests).
type ServiceOptions struct {
	AdditionalRootCAPEM []string
}

// GuestConfig is injected into guest config.json when proxy mode is enabled.
type GuestConfig struct {
	Enabled   bool   `json:"enabled"`
	ProxyURL  string `json:"proxy_url"`
	CACertPEM string `json:"ca_cert_pem"`
}
