package ingress

import (
	"fmt"
	"regexp"
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
	// Hostname is the hostname to match. Can be:
	// - Literal: "api.example.com" (exact match on Host header)
	// - Pattern: "{instance}.example.com" (dynamic, extracts subdomain as instance name)
	// This is required.
	Hostname string `json:"hostname"`

	// Port is the host port to listen on for this rule.
	// If not specified, defaults to 80.
	Port int `json:"port,omitempty"`

	// PathPrefix is the path prefix to match (optional, for future L7 routing).
	// If empty, matches all paths.
	// PathPrefix string `json:"path_prefix,omitempty"`
}

// captureRegex matches {name} captures in hostname patterns
var captureRegex = regexp.MustCompile(`\{([a-zA-Z_][a-zA-Z0-9_]*)\}`)

// IsPattern returns true if the hostname contains {name} captures.
func (m *IngressMatch) IsPattern() bool {
	return captureRegex.MatchString(m.Hostname)
}

// HostnamePattern represents a parsed hostname pattern with captures.
type HostnamePattern struct {
	// Original is the original pattern string (e.g., "{instance}.example.com")
	Original string

	// Wildcard is the Caddy wildcard pattern (e.g., "*.example.com")
	Wildcard string

	// Captures is the list of capture names in order (e.g., ["instance"])
	Captures []string

	// CaddyLabels maps capture names to Caddy placeholder expressions
	// e.g., {"instance": "{http.request.host.labels.2}"}
	CaddyLabels map[string]string
}

// ParsePattern parses the hostname pattern and returns a HostnamePattern.
// For "{instance}.example.com":
//   - Wildcard: "*.example.com"
//   - Captures: ["instance"]
//   - CaddyLabels: {"instance": "{http.request.host.labels.2}"}
//
// Caddy labels are indexed from the right (TLD first):
// - foo.bar.example.com → labels.0=com, labels.1=example, labels.2=bar, labels.3=foo
func (m *IngressMatch) ParsePattern() (*HostnamePattern, error) {
	if !m.IsPattern() {
		return nil, fmt.Errorf("hostname %q is not a pattern", m.Hostname)
	}

	// Split hostname into parts
	parts := strings.Split(m.Hostname, ".")
	if len(parts) < 2 {
		return nil, fmt.Errorf("hostname pattern %q must have at least two parts", m.Hostname)
	}

	captures := []string{}
	caddyLabels := make(map[string]string)
	wildcardParts := make([]string, len(parts))

	// Process each part and build wildcard + label mappings
	// Parts are indexed left-to-right, but Caddy labels are indexed right-to-left
	for i, part := range parts {
		// Caddy label index (from right)
		labelIndex := len(parts) - 1 - i

		matches := captureRegex.FindStringSubmatch(part)
		if matches != nil {
			// This part has a capture
			captureName := matches[1]

			// Check if this is a pure capture (entire part is {name}) or mixed
			if part == matches[0] {
				// Pure capture - replace with wildcard
				wildcardParts[i] = "*"
			} else {
				// Mixed capture (e.g., "api-{instance}") - not supported for now
				return nil, fmt.Errorf("mixed captures like %q are not supported, use pure captures like {name}", part)
			}

			// Check for duplicate capture names
			if _, exists := caddyLabels[captureName]; exists {
				return nil, fmt.Errorf("duplicate capture name %q in pattern", captureName)
			}

			captures = append(captures, captureName)
			caddyLabels[captureName] = fmt.Sprintf("{http.request.host.labels.%d}", labelIndex)
		} else {
			// Literal part - keep as-is
			wildcardParts[i] = part
		}
	}

	return &HostnamePattern{
		Original:    m.Hostname,
		Wildcard:    strings.Join(wildcardParts, "."),
		Captures:    captures,
		CaddyLabels: caddyLabels,
	}, nil
}

// ResolveInstance resolves the target instance expression using the pattern's captures.
// For a target like "{instance}" and captures {"instance": "{http.request.host.labels.2}"},
// returns "{http.request.host.labels.2}".
// For a literal target like "my-api", returns "my-api".
func (p *HostnamePattern) ResolveInstance(targetInstance string) string {
	result := targetInstance
	for captureName, caddyLabel := range p.CaddyLabels {
		result = strings.ReplaceAll(result, "{"+captureName+"}", caddyLabel)
	}
	return result
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

		// Check if hostname is a pattern or literal
		if rule.Match.IsPattern() {
			// Validate pattern syntax
			pattern, err := rule.Match.ParsePattern()
			if err != nil {
				return &ValidationError{Field: "rules", Message: fmt.Sprintf("invalid hostname pattern in rule %d: %v", i, err)}
			}

			// For patterns, target.instance must reference a capture
			if !captureRegex.MatchString(rule.Target.Instance) {
				return &ValidationError{Field: "rules", Message: fmt.Sprintf("pattern hostname in rule %d requires target.instance to reference a capture (e.g., {instance})", i)}
			}

			// Verify all captures in target.instance exist in the pattern
			targetCaptures := captureRegex.FindAllStringSubmatch(rule.Target.Instance, -1)
			for _, match := range targetCaptures {
				captureName := match[1]
				if _, exists := pattern.CaddyLabels[captureName]; !exists {
					return &ValidationError{Field: "rules", Message: fmt.Sprintf("target.instance in rule %d references unknown capture {%s}", i, captureName)}
				}
			}
		} else {
			// Literal hostname - disallow raw wildcards (only patterns supported)
			if strings.HasPrefix(rule.Match.Hostname, "*") {
				return &ValidationError{Field: "rules", Message: "wildcard hostnames are not supported, use pattern syntax like {instance}.example.com in rule " + strconv.Itoa(i)}
			}
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
