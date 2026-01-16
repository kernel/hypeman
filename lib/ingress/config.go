package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/kernel/hypeman/lib/dns"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/paths"
)

// DNSProvider represents supported DNS providers for ACME challenges.
type DNSProvider string

const (
	// DNSProviderNone indicates no DNS provider is configured.
	DNSProviderNone DNSProvider = ""
	// DNSProviderCloudflare uses Cloudflare for DNS challenges.
	DNSProviderCloudflare DNSProvider = "cloudflare"
)

// Caddy DNS module provider names (used in Caddy JSON config).
// These map our DNSProvider constants to the names expected by caddy-dns modules.
const (
	caddyProviderCloudflare = "cloudflare"
)

// SupportedDNSProviders returns a comma-separated list of supported DNS provider names.
// Used in error messages to keep them in sync as new providers are added.
func SupportedDNSProviders() string {
	return string(DNSProviderCloudflare)
}

// ParseDNSProvider parses a string into a DNSProvider, returning an error for unknown values.
func ParseDNSProvider(s string) (DNSProvider, error) {
	switch s {
	case "":
		return DNSProviderNone, nil
	case string(DNSProviderCloudflare):
		return DNSProviderCloudflare, nil
	default:
		return DNSProviderNone, fmt.Errorf("unknown DNS provider %q: supported providers are: %s", s, SupportedDNSProviders())
	}
}

// ACMEConfig holds ACME/TLS configuration for Caddy.
type ACMEConfig struct {
	// Email is the ACME account email (required for TLS).
	Email string

	// DNSProvider is the DNS provider for ACME challenges.
	DNSProvider DNSProvider

	// CA is the ACME CA URL. Empty means Let's Encrypt production.
	CA string

	// DNS propagation settings (applies to all providers)
	DNSPropagationTimeout string // Max time to wait for DNS propagation (e.g., "2m")
	DNSResolvers          string // Comma-separated DNS resolvers to use for checking propagation

	// AllowedDomains is a comma-separated list of domain patterns allowed for TLS ingresses.
	// Supports wildcards like "*.example.com" and exact matches like "api.example.com".
	// If empty, no TLS domains are allowed.
	AllowedDomains string

	// Cloudflare API token (if DNSProvider=cloudflare).
	CloudflareAPIToken string
}

// IsDomainAllowed checks if a hostname is allowed for TLS based on the AllowedDomains config.
// Returns true if the hostname matches any of the allowed patterns.
//
// Supported pattern types:
//   - Exact match: "api.example.com" matches only "api.example.com"
//   - Global wildcard: "*" matches any hostname (use with caution)
//   - Subdomain wildcard: "*.example.com" matches single-level subdomains only
//
// Wildcard behavior for "*.example.com":
//   - Matches: "foo.example.com", "bar.example.com"
//   - Does NOT match: "example.com" (apex domain)
//   - Does NOT match: "foo.bar.example.com" (multi-level subdomain)
func (c *ACMEConfig) IsDomainAllowed(hostname string) bool {
	if c.AllowedDomains == "" {
		return false // No domains allowed if not configured
	}

	patterns := strings.Split(c.AllowedDomains, ",")
	for _, pattern := range patterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}

		// Exact match
		if pattern == hostname {
			return true
		}

		// Global wildcard "*" - matches any domain (use with caution)
		if pattern == "*" {
			return true
		}

		// Subdomain wildcard match (e.g., "*.example.com" matches "foo.example.com")
		// Requirements:
		// - Pattern must start with "*." (e.g., "*.example.com")
		// - Hostname must end with the suffix (e.g., ".example.com")
		// - Hostname must have exactly one label before the suffix (single-level only)
		if strings.HasPrefix(pattern, "*.") {
			suffix := pattern[1:] // Remove the "*", keep ".example.com"
			if strings.HasSuffix(hostname, suffix) {
				// Extract the prefix (e.g., "foo" from "foo.example.com")
				prefix := strings.TrimSuffix(hostname, suffix)
				// Prefix must be non-empty and contain no dots (single-level subdomain only)
				if prefix != "" && !strings.Contains(prefix, ".") {
					return true
				}
			}
		}
	}

	return false
}

// IsTLSConfigured returns true if ACME/TLS is properly configured.
func (c *ACMEConfig) IsTLSConfigured() bool {
	if c.Email == "" || c.DNSProvider == DNSProviderNone {
		return false
	}

	switch c.DNSProvider {
	case DNSProviderCloudflare:
		return c.CloudflareAPIToken != ""
	default:
		return false
	}
}

// APIIngressConfig holds configuration for exposing the Hypeman API via Caddy.
type APIIngressConfig struct {
	// Hostname is the hostname for API access (e.g., "api.hostname.kernel.sh").
	// Empty means API ingress is disabled.
	Hostname string

	// Port is the local port where the Hypeman API is running.
	Port int

	// TLS enables TLS for the API hostname.
	TLS bool

	// RedirectHTTP enables HTTP to HTTPS redirect for the API hostname.
	RedirectHTTP bool
}

// IsEnabled returns true if API ingress is configured.
func (c *APIIngressConfig) IsEnabled() bool {
	return c.Hostname != ""
}

// CaddyConfigGenerator generates Caddy configuration from ingress resources.
type CaddyConfigGenerator struct {
	paths           *paths.Paths
	listenAddress   string
	adminAddress    string
	adminPort       int
	acme            ACMEConfig
	apiIngress      APIIngressConfig
	dnsResolverPort int
}

// NewCaddyConfigGenerator creates a new Caddy config generator.
func NewCaddyConfigGenerator(p *paths.Paths, listenAddress string, adminAddress string, adminPort int, acme ACMEConfig, apiIngress APIIngressConfig, dnsResolverPort int) *CaddyConfigGenerator {
	return &CaddyConfigGenerator{
		paths:           p,
		listenAddress:   listenAddress,
		adminAddress:    adminAddress,
		adminPort:       adminPort,
		acme:            acme,
		apiIngress:      apiIngress,
		dnsResolverPort: dnsResolverPort,
	}
}

// GenerateConfig generates the Caddy JSON configuration.
func (g *CaddyConfigGenerator) GenerateConfig(ctx context.Context, ingresses []Ingress) ([]byte, error) {
	config := g.buildConfig(ctx, ingresses)
	return json.MarshalIndent(config, "", "  ")
}

// buildConfig builds the complete Caddy configuration.
func (g *CaddyConfigGenerator) buildConfig(ctx context.Context, ingresses []Ingress) map[string]interface{} {
	log := logger.FromContext(ctx)

	// Build routes from ingresses
	routes := []interface{}{}
	redirectRoutes := []interface{}{}
	tlsHostnames := []string{}
	listenPorts := map[int]bool{}

	for _, ingress := range ingresses {
		for _, rule := range ingress.Rules {
			port := rule.Match.GetPort()
			listenPorts[port] = true

			// Determine hostname pattern (wildcard or literal) and instance expression
			var hostnameMatch string
			var instanceExpr string

			if rule.Match.IsPattern() {
				// Pattern hostname - parse and use wildcard + Caddy placeholders
				pattern, err := rule.Match.ParsePattern()
				if err != nil {
					log.WarnContext(ctx, "skipping ingress rule: invalid hostname pattern",
						"ingress_id", ingress.ID,
						"ingress_name", ingress.Name,
						"hostname", rule.Match.Hostname,
						"error", err)
					continue
				}
				hostnameMatch = pattern.Wildcard
				instanceExpr = pattern.ResolveInstance(rule.Target.Instance)
			} else {
				// Literal hostname - exact match
				hostnameMatch = rule.Match.Hostname
				instanceExpr = rule.Target.Instance
			}

			// Build DNS hostname for instance resolution
			// The instance expression may be a Caddy placeholder like {http.request.host.labels.2}
			// This becomes e.g., "my-api.hypeman.internal" or "{http.request.host.labels.2}.hypeman.internal"
			dnsHostname := fmt.Sprintf("%s.%s", instanceExpr, dns.Suffix)

			// Build the route with DNS-based dynamic upstreams using the "a" module
			reverseProxy := map[string]interface{}{
				"handler": "reverse_proxy",
				"dynamic_upstreams": map[string]interface{}{
					"source": "a",
					"name":   dnsHostname,
					"port":   fmt.Sprintf("%d", rule.Target.Port),
					"resolver": map[string]interface{}{
						"addresses": []string{fmt.Sprintf("127.0.0.1:%d", g.dnsResolverPort)},
					},
				},
			}

			route := map[string]interface{}{
				"match": []interface{}{
					map[string]interface{}{
						"host": []string{hostnameMatch},
					},
				},
				"handle": []interface{}{reverseProxy},
			}

			// Add terminal to stop processing after this route matches
			route["terminal"] = true

			routes = append(routes, route)

			// Track TLS hostnames for automation policy
			// For patterns, use the wildcard for TLS (e.g., "*.example.com")
			if rule.TLS {
				tlsHostnames = append(tlsHostnames, hostnameMatch)

				// Add HTTP redirect route if requested
				// Uses protocol matcher to only redirect HTTP, not HTTPS (which would cause redirect loop)
				if rule.RedirectHTTP {
					listenPorts[80] = true
					redirectRoute := map[string]interface{}{
						"match": []interface{}{
							map[string]interface{}{
								"host":     []string{hostnameMatch},
								"protocol": "http",
							},
						},
						"handle": []interface{}{
							map[string]interface{}{
								"handler": "static_response",
								"headers": map[string]interface{}{
									"Location": []string{"https://{http.request.host}{http.request.uri}"},
								},
								"status_code": 301,
							},
						},
						"terminal": true,
					}
					redirectRoutes = append(redirectRoutes, redirectRoute)
				}
			}
		}
	}

	// Add API ingress route if configured
	// This routes requests to the API hostname directly to localhost (Hypeman API)
	// IMPORTANT: API route must be prepended to routes so it takes precedence over
	// wildcard patterns that might otherwise match the API hostname
	if g.apiIngress.IsEnabled() {
		log.InfoContext(ctx, "adding API ingress route", "hostname", g.apiIngress.Hostname, "port", g.apiIngress.Port)

		// API reverse proxy to localhost
		apiReverseProxy := map[string]interface{}{
			"handler": "reverse_proxy",
			"upstreams": []map[string]interface{}{
				{"dial": fmt.Sprintf("127.0.0.1:%d", g.apiIngress.Port)},
			},
		}

		apiRoute := map[string]interface{}{
			"match": []interface{}{
				map[string]interface{}{
					"host": []string{g.apiIngress.Hostname},
				},
			},
			"handle":   []interface{}{apiReverseProxy},
			"terminal": true,
		}
		// Prepend API route so it takes precedence over wildcards
		routes = append([]interface{}{apiRoute}, routes...)

		// Add TLS configuration for API hostname
		if g.apiIngress.TLS {
			listenPorts[443] = true
			tlsHostnames = append(tlsHostnames, g.apiIngress.Hostname)

			// Add HTTP to HTTPS redirect for API hostname
			// Prepend so it takes precedence over wildcard redirects
			if g.apiIngress.RedirectHTTP {
				listenPorts[80] = true
				apiRedirectRoute := map[string]interface{}{
					"match": []interface{}{
						map[string]interface{}{
							"host":     []string{g.apiIngress.Hostname},
							"protocol": "http",
						},
					},
					"handle": []interface{}{
						map[string]interface{}{
							"handler": "static_response",
							"headers": map[string]interface{}{
								"Location": []string{"https://{http.request.host}{http.request.uri}"},
							},
							"status_code": 301,
						},
					},
					"terminal": true,
				}
				redirectRoutes = append([]interface{}{apiRedirectRoute}, redirectRoutes...)
			}
		} else {
			listenPorts[80] = true
		}
	}

	// Build listen addresses (sorted for deterministic config output)
	ports := make([]int, 0, len(listenPorts))
	for port := range listenPorts {
		ports = append(ports, port)
	}
	sort.Ints(ports)
	listenAddrs := make([]string, 0, len(ports))
	for _, port := range ports {
		listenAddrs = append(listenAddrs, fmt.Sprintf("%s:%d", g.listenAddress, port))
	}

	// Build base config (admin API only)
	// Caddy writes JSON logs to stderr by default, which we capture to caddy.log
	config := map[string]interface{}{
		"admin": map[string]interface{}{
			"listen": fmt.Sprintf("%s:%d", g.adminAddress, g.adminPort),
		},
	}

	// Only add HTTP server if we have listen addresses (i.e., ingresses exist)
	if len(listenAddrs) > 0 {
		// Build server configuration
		server := map[string]interface{}{
			"listen": listenAddrs,
		}

		// Combine redirect routes (for HTTP) and main routes
		// Use slices.Concat to avoid modifying original slices
		allRoutes := slices.Concat(redirectRoutes, routes)

		// Add catch-all route at the end to return 404 for unmatched hostnames
		// This must be last since routes are evaluated in order
		catchAllRoute := map[string]interface{}{
			"handle": []interface{}{
				map[string]interface{}{
					"handler":     "static_response",
					"status_code": 404,
					"headers": map[string]interface{}{
						"Content-Type": []string{"text/plain; charset=utf-8"},
					},
					"body": "Not Found: no ingress configured for hostname {http.request.host}",
				},
			},
		}
		allRoutes = append(allRoutes, catchAllRoute)

		server["routes"] = allRoutes

		// Configure automatic HTTPS settings
		if len(tlsHostnames) > 0 {
			// When we have TLS hostnames, disable only redirects - we handle them explicitly
			server["automatic_https"] = map[string]interface{}{
				"disable_redirects": true,
			}
		} else {
			// No TLS hostnames - disable automatic HTTPS completely
			server["automatic_https"] = map[string]interface{}{
				"disable": true,
			}
		}

		// Disable access logs (per-request logs) - we only want system logs
		server["logs"] = map[string]interface{}{}

		config["apps"] = map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"ingress": server,
				},
			},
		}
	}

	// Add TLS automation if we have TLS hostnames
	if len(tlsHostnames) > 0 && g.acme.IsTLSConfigured() {
		if config["apps"] == nil {
			config["apps"] = map[string]interface{}{}
		}
		config["apps"].(map[string]interface{})["tls"] = g.buildTLSConfig(tlsHostnames)
	}

	// Configure Caddy storage paths
	config["storage"] = map[string]interface{}{
		"module": "file_system",
		"root":   g.paths.CaddyDataDir(),
	}

	return config
}

// buildTLSConfig builds the TLS automation configuration.
func (g *CaddyConfigGenerator) buildTLSConfig(hostnames []string) map[string]interface{} {
	issuer := map[string]interface{}{
		"module": "acme",
		"email":  g.acme.Email,
	}

	// Set CA if specified (otherwise uses Let's Encrypt production)
	if g.acme.CA != "" {
		issuer["ca"] = g.acme.CA
	}

	// Configure DNS challenge based on provider
	issuer["challenges"] = map[string]interface{}{
		"dns": g.buildDNSChallengeConfig(),
	}

	return map[string]interface{}{
		"automation": map[string]interface{}{
			"policies": []interface{}{
				map[string]interface{}{
					"subjects": hostnames,
					"issuers":  []interface{}{issuer},
				},
			},
		},
	}
}

// buildDNSChallengeConfig builds the DNS challenge configuration.
// Uses the caddy-dns module format: https://github.com/caddy-dns/cloudflare
func (g *CaddyConfigGenerator) buildDNSChallengeConfig() map[string]interface{} {
	dnsConfig := map[string]interface{}{}

	// Add provider-specific configuration
	switch g.acme.DNSProvider {
	case DNSProviderCloudflare:
		// caddy-dns/cloudflare module format
		dnsConfig["provider"] = map[string]interface{}{
			"name":      caddyProviderCloudflare,
			"api_token": g.acme.CloudflareAPIToken,
		}
	default:
		// This shouldn't happen due to validation at startup, but log if it does
		slog.Warn("unknown DNS provider in buildDNSChallengeConfig", "provider", g.acme.DNSProvider)
		return map[string]interface{}{}
	}

	// Add propagation settings (applies to all providers)
	if g.acme.DNSPropagationTimeout != "" {
		dnsConfig["propagation_timeout"] = g.acme.DNSPropagationTimeout
	}
	if g.acme.DNSResolvers != "" {
		// Split comma-separated resolvers into array
		resolvers := strings.Split(g.acme.DNSResolvers, ",")
		for i := range resolvers {
			resolvers[i] = strings.TrimSpace(resolvers[i])
		}
		dnsConfig["resolvers"] = resolvers
	}

	return dnsConfig
}

// WriteConfig writes the Caddy configuration to disk.
func (g *CaddyConfigGenerator) WriteConfig(ctx context.Context, ingresses []Ingress) error {
	configDir := filepath.Dir(g.paths.CaddyConfig())

	// Ensure the directory exists
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	// Ensure data directory exists
	if err := os.MkdirAll(g.paths.CaddyDataDir(), 0755); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}

	// Generate config
	data, err := g.GenerateConfig(ctx, ingresses)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	// Write atomically
	return g.atomicWrite(g.paths.CaddyConfig(), data)
}

// atomicWrite writes data to a file atomically using a temp file and rename.
func (g *CaddyConfigGenerator) atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tempFile, err := os.CreateTemp(dir, "caddy-*.json")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tempPath := tempFile.Name()

	// Clean up temp file on any error
	defer func() {
		if tempPath != "" {
			os.Remove(tempPath)
		}
	}()

	if _, err := tempFile.Write(data); err != nil {
		tempFile.Close()
		return fmt.Errorf("write temp file: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp file: %w", err)
	}

	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	tempPath = "" // Prevent cleanup of renamed file
	return nil
}

// HasTLSRules checks if any ingress has TLS enabled.
func HasTLSRules(ingresses []Ingress) bool {
	for _, ingress := range ingresses {
		for _, rule := range ingress.Rules {
			if rule.TLS {
				return true
			}
		}
	}
	return false
}
