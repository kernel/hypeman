package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

// DNSProvider represents supported DNS providers for ACME challenges.
type DNSProvider string

const (
	// DNSProviderNone indicates no DNS provider is configured.
	DNSProviderNone DNSProvider = ""
	// DNSProviderCloudflare uses Cloudflare for DNS challenges.
	DNSProviderCloudflare DNSProvider = "cloudflare"
	// DNSProviderRoute53 uses AWS Route53 for DNS challenges.
	DNSProviderRoute53 DNSProvider = "route53"
)

// ParseDNSProvider parses a string into a DNSProvider, returning an error for unknown values.
func ParseDNSProvider(s string) (DNSProvider, error) {
	switch s {
	case "":
		return DNSProviderNone, nil
	case "cloudflare":
		return DNSProviderCloudflare, nil
	case "route53":
		return DNSProviderRoute53, nil
	default:
		return DNSProviderNone, fmt.Errorf("unknown DNS provider %q: must be 'cloudflare' or 'route53'", s)
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

	// Cloudflare API token (if DNSProvider=cloudflare).
	CloudflareAPIToken string

	// AWS credentials (if DNSProvider=route53).
	AWSAccessKeyID     string
	AWSSecretAccessKey string
	AWSRegion          string
	AWSHostedZoneID    string
}

// IsTLSConfigured returns true if ACME/TLS is properly configured.
func (c *ACMEConfig) IsTLSConfigured() bool {
	if c.Email == "" || c.DNSProvider == DNSProviderNone {
		return false
	}

	switch c.DNSProvider {
	case DNSProviderCloudflare:
		return c.CloudflareAPIToken != ""
	case DNSProviderRoute53:
		return c.AWSAccessKeyID != "" && c.AWSSecretAccessKey != ""
	default:
		return false
	}
}

// CaddyConfigGenerator generates Caddy configuration from ingress resources.
type CaddyConfigGenerator struct {
	paths         *paths.Paths
	listenAddress string
	adminAddress  string
	adminPort     int
	acme          ACMEConfig
}

// NewCaddyConfigGenerator creates a new Caddy config generator.
func NewCaddyConfigGenerator(p *paths.Paths, listenAddress string, adminAddress string, adminPort int, acme ACMEConfig) *CaddyConfigGenerator {
	return &CaddyConfigGenerator{
		paths:         p,
		listenAddress: listenAddress,
		adminAddress:  adminAddress,
		adminPort:     adminPort,
		acme:          acme,
	}
}

// GenerateConfig generates the Caddy JSON configuration.
func (g *CaddyConfigGenerator) GenerateConfig(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) ([]byte, error) {
	config := g.buildConfig(ctx, ingresses, ipResolver)
	return json.MarshalIndent(config, "", "  ")
}

// buildConfig builds the complete Caddy configuration.
func (g *CaddyConfigGenerator) buildConfig(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) map[string]interface{} {
	log := logger.FromContext(ctx)

	// Build routes from ingresses
	routes := []interface{}{}
	redirectRoutes := []interface{}{}
	tlsHostnames := []string{}
	listenPorts := map[int]bool{}

	for _, ingress := range ingresses {
		for _, rule := range ingress.Rules {
			// Resolve instance IP
			ip, err := ipResolver(rule.Target.Instance)
			if err != nil {
				log.WarnContext(ctx, "skipping ingress rule: cannot resolve instance IP",
					"ingress_id", ingress.ID,
					"ingress_name", ingress.Name,
					"hostname", rule.Match.Hostname,
					"instance", rule.Target.Instance,
					"error", err)
				continue
			}

			port := rule.Match.GetPort()
			listenPorts[port] = true

			// Build the route
			route := map[string]interface{}{
				"match": []interface{}{
					map[string]interface{}{
						"host": []string{rule.Match.Hostname},
					},
				},
				"handle": []interface{}{
					map[string]interface{}{
						"handler": "reverse_proxy",
						"upstreams": []interface{}{
							map[string]interface{}{
								"dial": fmt.Sprintf("%s:%d", ip, rule.Target.Port),
							},
						},
					},
				},
			}

			// Add terminal to stop processing after this route matches
			route["terminal"] = true

			routes = append(routes, route)

			// Track TLS hostnames for automation policy
			if rule.TLS {
				tlsHostnames = append(tlsHostnames, rule.Match.Hostname)

				// Add HTTP redirect route if requested
				if rule.RedirectHTTP {
					listenPorts[80] = true
					redirectRoute := map[string]interface{}{
						"match": []interface{}{
							map[string]interface{}{
								"host": []string{rule.Match.Hostname},
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

	// Build listen addresses
	listenAddrs := []string{}
	for port := range listenPorts {
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
		if len(allRoutes) > 0 {
			server["routes"] = allRoutes
		}

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
func (g *CaddyConfigGenerator) buildDNSChallengeConfig() map[string]interface{} {
	switch g.acme.DNSProvider {
	case DNSProviderCloudflare:
		return map[string]interface{}{
			"provider": map[string]interface{}{
				"name":      "cloudflare",
				"api_token": g.acme.CloudflareAPIToken,
			},
		}
	case DNSProviderRoute53:
		provider := map[string]interface{}{
			"name":              "route53",
			"access_key_id":     g.acme.AWSAccessKeyID,
			"secret_access_key": g.acme.AWSSecretAccessKey,
			"region":            g.acme.AWSRegion,
		}
		if g.acme.AWSHostedZoneID != "" {
			provider["hosted_zone_id"] = g.acme.AWSHostedZoneID
		}
		return map[string]interface{}{
			"provider": provider,
		}
	default:
		// Should not happen - DNSProvider is validated at startup
		return map[string]interface{}{}
	}
}

// WriteConfig writes the Caddy configuration to disk.
func (g *CaddyConfigGenerator) WriteConfig(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) error {
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
	data, err := g.GenerateConfig(ctx, ingresses, ipResolver)
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
