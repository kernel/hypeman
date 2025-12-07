package ingress

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
)

// ACMEConfig holds ACME/TLS configuration for Caddy.
type ACMEConfig struct {
	// Email is the ACME account email (required for TLS).
	Email string

	// DNSProvider is the DNS provider for challenges: "cloudflare" or "route53".
	DNSProvider string

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
	if c.Email == "" || c.DNSProvider == "" {
		return false
	}

	switch c.DNSProvider {
	case "cloudflare":
		return c.CloudflareAPIToken != ""
	case "route53":
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

	// If no ingresses, still create a minimal server
	if len(listenAddrs) == 0 {
		listenAddrs = []string{fmt.Sprintf("%s:80", g.listenAddress)}
	}

	// Build server configuration
	server := map[string]interface{}{
		"listen": listenAddrs,
	}

	// Combine redirect routes (for HTTP) and main routes
	allRoutes := append(redirectRoutes, routes...)
	if len(allRoutes) > 0 {
		server["routes"] = allRoutes
	}

	// Add automatic HTTPS settings
	server["automatic_https"] = map[string]interface{}{
		// Disable automatic HTTPS redirects - we handle them explicitly
		"disable_redirects": true,
	}

	// Disable access logs (per-request logs) - we only want system logs
	server["logs"] = map[string]interface{}{}

	config := map[string]interface{}{
		"admin": map[string]interface{}{
			"listen": fmt.Sprintf("%s:%d", g.adminAddress, g.adminPort),
		},
		// Configure logging: system logs only (no access logs)
		"logging": map[string]interface{}{
			"logs": map[string]interface{}{
				"default": map[string]interface{}{
					"writer": map[string]interface{}{
						"output":   "file",
						"filename": g.paths.CaddySystemLog(),
					},
					"encoder": map[string]interface{}{
						"format": "json",
					},
					"level": "INFO",
				},
			},
		},
		"apps": map[string]interface{}{
			"http": map[string]interface{}{
				"servers": map[string]interface{}{
					"ingress": server,
				},
			},
		},
	}

	// Add TLS automation if we have TLS hostnames
	if len(tlsHostnames) > 0 && g.acme.IsTLSConfigured() {
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
	case "cloudflare":
		return map[string]interface{}{
			"provider": map[string]interface{}{
				"name":      "cloudflare",
				"api_token": g.acme.CloudflareAPIToken,
			},
		}
	case "route53":
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
