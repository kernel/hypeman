package ingress

import (
	"fmt"
	"os"
	"strings"

	"github.com/onkernel/hypeman/lib/paths"
	"gopkg.in/yaml.v3"
)

// EnvoyConfigGenerator generates Envoy configuration from ingress resources.
type EnvoyConfigGenerator struct {
	paths         *paths.Paths
	listenAddress string
	listenPort    int
	adminAddress  string
	adminPort     int
}

// NewEnvoyConfigGenerator creates a new config generator.
func NewEnvoyConfigGenerator(p *paths.Paths, listenAddress string, listenPort int, adminAddress string, adminPort int) *EnvoyConfigGenerator {
	return &EnvoyConfigGenerator{
		paths:         p,
		listenAddress: listenAddress,
		listenPort:    listenPort,
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

// buildListeners builds the listeners configuration.
func (g *EnvoyConfigGenerator) buildListeners(ingresses []Ingress, ipResolver func(instance string) (string, error)) []interface{} {
	if len(ingresses) == 0 {
		return []interface{}{}
	}

	// Build filter chains for each hostname
	filterChains := g.buildFilterChains(ingresses, ipResolver)
	if len(filterChains) == 0 {
		return []interface{}{}
	}

	listener := map[string]interface{}{
		"name": "ingress_listener",
		"address": map[string]interface{}{
			"socket_address": map[string]interface{}{
				"address":    g.listenAddress,
				"port_value": g.listenPort,
			},
		},
		"filter_chains": filterChains,
	}

	return []interface{}{listener}
}

// buildFilterChains builds filter chains for hostname-based routing.
func (g *EnvoyConfigGenerator) buildFilterChains(ingresses []Ingress, ipResolver func(instance string) (string, error)) []interface{} {
	var filterChains []interface{}

	for _, ingress := range ingresses {
		for _, rule := range ingress.Rules {
			// Resolve instance IP
			ip, err := ipResolver(rule.Target.Instance)
			if err != nil {
				// Skip rules where we can't resolve the instance
				continue
			}

			clusterName := g.clusterName(ingress.ID, rule.Target.Instance, rule.Target.Port)

			// Build route configuration for this hostname
			routeConfig := map[string]interface{}{
				"name": fmt.Sprintf("route_%s_%s", ingress.ID, sanitizeHostname(rule.Match.Hostname)),
				"virtual_hosts": []interface{}{
					map[string]interface{}{
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
					},
				},
			}

			httpFilters := []interface{}{
				map[string]interface{}{
					"name": "envoy.filters.http.router",
					"typed_config": map[string]interface{}{
						"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
					},
				},
			}

			httpConnectionManager := map[string]interface{}{
				"@type":        "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
				"stat_prefix":  fmt.Sprintf("ingress_%s", ingress.ID),
				"codec_type":   "AUTO",
				"route_config": routeConfig,
				"http_filters": httpFilters,
			}

			filterChain := map[string]interface{}{
				"filter_chain_match": map[string]interface{}{
					"server_names": []string{rule.Match.Hostname},
				},
				"filters": []interface{}{
					map[string]interface{}{
						"name":         "envoy.filters.network.http_connection_manager",
						"typed_config": httpConnectionManager,
					},
				},
			}

			// Include resolved IP for documentation purposes in comments
			_ = ip

			filterChains = append(filterChains, filterChain)
		}
	}

	// Add a default filter chain for non-matching requests (returns 404)
	if len(filterChains) > 0 {
		defaultRouteConfig := map[string]interface{}{
			"name": "default_route",
			"virtual_hosts": []interface{}{
				map[string]interface{}{
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
				},
			},
		}

		defaultHttpConnectionManager := map[string]interface{}{
			"@type":        "type.googleapis.com/envoy.extensions.filters.network.http_connection_manager.v3.HttpConnectionManager",
			"stat_prefix":  "ingress_default",
			"codec_type":   "AUTO",
			"route_config": defaultRouteConfig,
			"http_filters": []interface{}{
				map[string]interface{}{
					"name": "envoy.filters.http.router",
					"typed_config": map[string]interface{}{
						"@type": "type.googleapis.com/envoy.extensions.filters.http.router.v3.Router",
					},
				},
			},
		}

		defaultFilterChain := map[string]interface{}{
			"filters": []interface{}{
				map[string]interface{}{
					"name":         "envoy.filters.network.http_connection_manager",
					"typed_config": defaultHttpConnectionManager,
				},
			},
		}

		filterChains = append(filterChains, defaultFilterChain)
	}

	return filterChains
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
