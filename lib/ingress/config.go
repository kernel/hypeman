package ingress

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/onkernel/hypeman/lib/paths"
	"gopkg.in/yaml.v3"
)

// EnvoyConfigGenerator generates Envoy configuration from ingress resources.
type EnvoyConfigGenerator struct {
	paths         *paths.Paths
	listenAddress string
	adminAddress  string
	adminPort     int
}

// NewEnvoyConfigGenerator creates a new config generator.
func NewEnvoyConfigGenerator(p *paths.Paths, listenAddress string, adminAddress string, adminPort int) *EnvoyConfigGenerator {
	return &EnvoyConfigGenerator{
		paths:         p,
		listenAddress: listenAddress,
		adminAddress:  adminAddress,
		adminPort:     adminPort,
	}
}

// GenerateConfig generates Envoy configuration from the given ingresses and their resolved IP addresses.
// The ipResolver function takes an instance name/ID and returns (ip, error).
func (g *EnvoyConfigGenerator) GenerateConfig(ingresses []Ingress, ipResolver func(instance string) (string, error)) ([]byte, error) {
	config := g.buildConfig(ingresses, ipResolver)
	return yaml.Marshal(config)
}

// WriteConfig generates and writes the Envoy configuration file.
func (g *EnvoyConfigGenerator) WriteConfig(ingresses []Ingress, ipResolver func(instance string) (string, error)) error {
	data, err := g.GenerateConfig(ingresses, ipResolver)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	configPath := g.paths.EnvoyConfig()

	// Ensure the directory exists
	if err := os.MkdirAll(filepath.Dir(configPath), 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	if err := os.WriteFile(configPath, data, 0644); err != nil {
		return fmt.Errorf("write config: %w", err)
	}

	return nil
}

// buildConfig builds the Envoy configuration structure.
func (g *EnvoyConfigGenerator) buildConfig(ingresses []Ingress, ipResolver func(instance string) (string, error)) map[string]interface{} {
	config := map[string]interface{}{
		"admin": map[string]interface{}{
			"address": map[string]interface{}{
				"socket_address": map[string]interface{}{
					"address":    g.adminAddress,
					"port_value": g.adminPort,
				},
			},
		},
		"static_resources": map[string]interface{}{
			"listeners": g.buildListeners(ingresses, ipResolver),
			"clusters":  g.buildClusters(ingresses, ipResolver),
		},
	}

	return config
}

// buildListeners builds the listeners configuration - one per unique port.
func (g *EnvoyConfigGenerator) buildListeners(ingresses []Ingress, ipResolver func(instance string) (string, error)) []interface{} {
	if len(ingresses) == 0 {
		return []interface{}{}
	}

	// Group rules by port
	portToFilterChains := g.buildFilterChainsByPort(ingresses, ipResolver)
	if len(portToFilterChains) == 0 {
		return []interface{}{}
	}

	// Create one listener per port
	var listeners []interface{}
	for port, filterChains := range portToFilterChains {
		listener := map[string]interface{}{
			"name": fmt.Sprintf("ingress_listener_%d", port),
			"address": map[string]interface{}{
				"socket_address": map[string]interface{}{
					"address":    g.listenAddress,
					"port_value": port,
				},
			},
			"filter_chains": filterChains,
		}
		listeners = append(listeners, listener)
	}

	return listeners
}

// buildFilterChainsByPort builds filter chains grouped by port for hostname-based routing.
// For plain HTTP, we use virtual hosts with domain matching (Host header) instead of
// filter_chain_match with server_names (which only works for TLS/SNI).
func (g *EnvoyConfigGenerator) buildFilterChainsByPort(ingresses []Ingress, ipResolver func(instance string) (string, error)) map[int][]interface{} {
	// Group virtual hosts by port
	portToVirtualHosts := make(map[int][]interface{})

	for _, ingress := range ingresses {
		for _, rule := range ingress.Rules {
			// Resolve instance IP - skip rules where we can't resolve
			_, err := ipResolver(rule.Target.Instance)
			if err != nil {
				continue
			}

			port := rule.Match.GetPort()
			clusterName := g.clusterName(ingress.ID, rule.Target.Instance, rule.Target.Port)

			// Build virtual host for this hostname
			virtualHost := map[string]interface{}{
				"name":    fmt.Sprintf("vh_%s_%s", ingress.ID, sanitizeHostname(rule.Match.Hostname)),
				"domains": []string{rule.Match.Hostname},
				"routes": []interface{}{
					map[string]interface{}{
						"match": map[string]interface{}{
							"prefix": "/",
						},
						"route": map[string]interface{}{
							"cluster": clusterName,
						},
					},
				},
			}

			portToVirtualHosts[port] = append(portToVirtualHosts[port], virtualHost)
		}
	}

	// Build filter chains - one per port with all virtual hosts combined
	portToFilterChains := make(map[int][]interface{})

	for port, virtualHosts := range portToVirtualHosts {
		// Add default virtual host for unmatched hostnames (returns 404)
		defaultVirtualHost := map[string]interface{}{
			"name":    "default",
			"domains": []string{"*"},
			"routes": []interface{}{
				map[string]interface{}{
					"match": map[string]interface{}{
						"prefix": "/",
					},
					"direct_response": map[string]interface{}{
						"status": 404,
						"body": map[string]interface{}{
							"inline_string": "No ingress found for this hostname",
						},
					},
				},
			},
		}
		allVirtualHosts := append(virtualHosts, defaultVirtualHost)

		routeConfig := map[string]interface{}{
			"name":          fmt.Sprintf("ingress_routes_%d", port),
			"virtual_hosts": allVirtualHosts,
		}

		httpConnectionManager := map[string]interface{}{
			"@type":        "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
			"stat_prefix":  fmt.Sprintf("ingress_%d", port),
			"codec_type":   "AUTO",
			"route_config": routeConfig,
			"http_filters": []interface{}{
				map[string]interface{}{
					"name": "envoy.filters.http.router",
					"typed_config": map[string]interface{}{
						"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
					},
				},
			},
		}

		filterChain := map[string]interface{}{
			"filters": []interface{}{
				map[string]interface{}{
					"name":         "envoy.filters.network.http_connection_manager",
					"typed_config": httpConnectionManager,
				},
			},
		}

		portToFilterChains[port] = []interface{}{filterChain}
	}

	return portToFilterChains
}

// buildClusters builds the clusters configuration.
func (g *EnvoyConfigGenerator) buildClusters(ingresses []Ingress, ipResolver func(instance string) (string, error)) []interface{} {
	var clusters []interface{}
	seen := make(map[string]bool)

	for _, ingress := range ingresses {
		for _, rule := range ingress.Rules {
			clusterName := g.clusterName(ingress.ID, rule.Target.Instance, rule.Target.Port)
			if seen[clusterName] {
				continue
			}
			seen[clusterName] = true

			// Resolve instance IP
			ip, err := ipResolver(rule.Target.Instance)
			if err != nil {
				// Skip clusters where we can't resolve the instance
				continue
			}

			cluster := map[string]interface{}{
				"name":            clusterName,
				"connect_timeout": "5s",
				"type":            "STATIC",
				"lb_policy":       "ROUND_ROBIN",
				"load_assignment": map[string]interface{}{
					"cluster_name": clusterName,
					"endpoints": []interface{}{
						map[string]interface{}{
							"lb_endpoints": []interface{}{
								map[string]interface{}{
									"endpoint": map[string]interface{}{
										"address": map[string]interface{}{
											"socket_address": map[string]interface{}{
												"address":    ip,
												"port_value": rule.Target.Port,
											},
										},
									},
								},
							},
						},
					},
				},
			}

			clusters = append(clusters, cluster)
		}
	}

	return clusters
}

// clusterName generates a unique cluster name for an ingress target.
func (g *EnvoyConfigGenerator) clusterName(ingressID, instance string, port int) string {
	return fmt.Sprintf("ingress_%s_%s_%d", ingressID, sanitizeName(instance), port)
}

// sanitizeHostname converts a hostname to a safe string for use in names.
func sanitizeHostname(hostname string) string {
	return strings.ReplaceAll(strings.ReplaceAll(hostname, ".", "_"), "-", "_")
}

// sanitizeName converts a name to a safe string for use in Envoy config names.
func sanitizeName(name string) string {
	return strings.ReplaceAll(strings.ReplaceAll(name, ".", "_"), "-", "_")
}
