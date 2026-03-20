package egressproxy

import (
	"errors"
	"log/slog"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

const (
	DefaultListenPort = 18080
)

var (
	ErrGatewayMismatch = errors.New("egress proxy already initialized with different gateway")
)

// InstanceConfig defines per-instance proxy behavior.
type InstanceConfig struct {
	InstanceID        string
	SourceIP          string
	TAPDevice         string
	BlockAllTCPEgress bool
	HeaderInjectRules []HeaderInjectRuleConfig
}

// HeaderInjectRuleConfig defines one host-managed outbound header injection policy.
type HeaderInjectRuleConfig struct {
	HeaderName     string
	HeaderValue    string
	AllowedDomains []string // optional exact or *.example.com patterns; empty means allow all
}

// ServiceOptions customizes service construction (primarily for tests).
type ServiceOptions struct {
	AdditionalRootCAPEM []string
	Logger              *slog.Logger
	Meter               metric.Meter
	Tracer              trace.Tracer
}

// GuestConfig is injected into guest config.json when proxy mode is enabled.
type GuestConfig struct {
	Enabled   bool   `json:"enabled"`
	ProxyURL  string `json:"proxy_url"`
	CACertPEM string `json:"ca_cert_pem"`
}
