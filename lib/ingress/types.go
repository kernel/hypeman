package ingress

import (
	"strconv"
	"strings"
	"time"
)

// Ingress represents an ingress resource that defines how external traffic
// should be routed to VM instances.
type Ingress struct {
	// ID is the unique identifier for this ingress (auto-generated).
	ID string `json:"id"`

	// Name is a human-readable name for the ingress.
	Name string `json:"name"`

	// Rules define the routing rules for this ingress.
	Rules []IngressRule `json:"rules"`

	// CreatedAt is the timestamp when this ingress was created.
	CreatedAt time.Time `json:"created_at"`
}

// IngressRule defines a single routing rule within an ingress.
type IngressRule struct {
	// Match specifies the conditions for matching incoming requests.
	Match IngressMatch `json:"match"`

	// Target specifies where matching requests should be routed.
	Target IngressTarget `json:"target"`

	// TLS enables TLS termination for this rule.
	// When enabled, a certificate will be automatically issued via ACME.
	TLS bool `json:"tls,omitempty"`

	// RedirectHTTP creates an automatic HTTP to HTTPS redirect for this hostname.
	// Only applies when TLS is enabled.
	RedirectHTTP bool `json:"redirect_http,omitempty"`
}

// IngressMatch specifies the conditions for matching incoming requests.
type IngressMatch struct {
	// Hostname is the hostname to match (exact match on Host header).
	// This is required.
	Hostname string `json:"hostname"`

	// Port is the host port to listen on for this rule.
	// If not specified, defaults to 80.
	Port int `json:"port,omitempty"`

	// PathPrefix is the path prefix to match (optional, for future L7 routing).
	// If empty, matches all paths.
	// PathPrefix string `json:"path_prefix,omitempty"`
}

// IngressTarget specifies the target for routing matched requests.
type IngressTarget struct {
	// Instance is the name or ID of the target instance.
	Instance string `json:"instance"`

	// Port is the port on the target instance.
	Port int `json:"port"`
}

// CreateIngressRequest is the request body for creating a new ingress.
type CreateIngressRequest struct {
	// Name is a human-readable name for the ingress.
	Name string `json:"name"`

	// Rules define the routing rules for this ingress.
	Rules []IngressRule `json:"rules"`
}

// Validate validates the CreateIngressRequest.
func (r *CreateIngressRequest) Validate() error {
	if r.Name == "" {
		return &ValidationError{Field: "name", Message: "name is required"}
	}

	if len(r.Rules) == 0 {
		return &ValidationError{Field: "rules", Message: "at least one rule is required"}
	}

	for i, rule := range r.Rules {
		if rule.Match.Hostname == "" {
			return &ValidationError{Field: "rules", Message: "hostname is required in rule " + strconv.Itoa(i)}
		}
		// Wildcard hostnames are not supported for ACME certificates
		if strings.HasPrefix(rule.Match.Hostname, "*") {
			return &ValidationError{Field: "rules", Message: "wildcard hostnames are not supported in rule " + strconv.Itoa(i)}
		}
		// Port is optional (defaults to 80), but if specified must be valid
		if rule.Match.Port != 0 && (rule.Match.Port < 1 || rule.Match.Port > 65535) {
			return &ValidationError{Field: "rules", Message: "match.port must be between 1 and 65535 in rule " + strconv.Itoa(i)}
		}
		if rule.Target.Instance == "" {
			return &ValidationError{Field: "rules", Message: "instance is required in rule " + strconv.Itoa(i)}
		}
		if rule.Target.Port <= 0 || rule.Target.Port > 65535 {
			return &ValidationError{Field: "rules", Message: "target.port must be between 1 and 65535 in rule " + strconv.Itoa(i)}
		}
		// redirect_http only makes sense with TLS
		if rule.RedirectHTTP && !rule.TLS {
			return &ValidationError{Field: "rules", Message: "redirect_http requires tls to be enabled in rule " + strconv.Itoa(i)}
		}
	}

	return nil
}

// GetPort returns the port for this match, defaulting to 80 if not specified.
func (m *IngressMatch) GetPort() int {
	if m.Port == 0 {
		return 80
	}
	return m.Port
}

// ValidationError represents a validation error.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Message
}
