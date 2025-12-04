package ingress

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/paths"
	"gopkg.in/yaml.v3"
)

// ConfigValidator validates Envoy configuration files.
type ConfigValidator interface {
	ValidateConfig(configPath string) error
}

// EnvoyConfigGenerator generates Envoy configuration from ingress resources.
type EnvoyConfigGenerator struct {
	paths         *paths.Paths
	listenAddress string
	adminAddress  string
	adminPort     int
	validator     ConfigValidator
	otel          OTELConfig
}

// NewEnvoyConfigGenerator creates a new config generator.
func NewEnvoyConfigGenerator(p *paths.Paths, listenAddress string, adminAddress string, adminPort int, validator ConfigValidator, otel OTELConfig) *EnvoyConfigGenerator {
	return &EnvoyConfigGenerator{
		paths:         p,
		listenAddress: listenAddress,
		adminAddress:  adminAddress,
		adminPort:     adminPort,
		validator:     validator,
		otel:          otel,
	}
}

// GenerateConfig generates Envoy configuration from the given ingresses and their resolved IP addresses.
// The ipResolver function takes an instance name/ID and returns (ip, error).
func (g *EnvoyConfigGenerator) GenerateConfig(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) ([]byte, error) {
	config := g.buildConfig(ctx, ingresses, ipResolver)
	return yaml.Marshal(config)
}

// WriteConfig generates, validates, and writes the Envoy configuration file.
// The config is written to a temp file first, validated, then atomically moved to the final path.
func (g *EnvoyConfigGenerator) WriteConfig(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) error {
	data, err := g.GenerateConfig(ctx, ingresses, ipResolver)
	if err != nil {
		return fmt.Errorf("generate config: %w", err)
	}

	configPath := g.paths.EnvoyConfig()
	configDir := filepath.Dir(configPath)

	// Ensure the directory exists
	if err := os.MkdirAll(configDir, 0755); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}

	// Write to a temp file first
	tempFile, err := os.CreateTemp(configDir, "envoy-config-*.yaml")
	if err != nil {
		return fmt.Errorf("create temp config file: %w", err)
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
		return fmt.Errorf("write temp config: %w", err)
	}
	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temp config: %w", err)
	}

	// Validate the config if validator is available
	if g.validator != nil {
		if err := g.validator.ValidateConfig(tempPath); err != nil {
			log := logger.FromContext(ctx)
			log.ErrorContext(ctx, "envoy config validation failed", "error", err)
			return ErrConfigValidationFailed
		}
	}

	// Atomically move temp file to final path
	if err := os.Rename(tempPath, configPath); err != nil {
		return fmt.Errorf("rename config file: %w", err)
	}

	// Clear tempPath so defer doesn't try to remove the renamed file
	tempPath = ""

	return nil
}

// buildConfig builds the Envoy configuration structure.
func (g *EnvoyConfigGenerator) buildConfig(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) map[string]interface{} {
	clusters := g.buildClusters(ctx, ingresses, ipResolver)

	// Add OTEL collector cluster if enabled (for metrics export)
	if g.otel.Enabled && g.otel.Endpoint != "" {
		otelCluster := g.buildOTELCollectorCluster()
		clusters = append(clusters, otelCluster)
	}

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
			"listeners": g.buildListeners(ctx, ingresses, ipResolver),
			"clusters":  clusters,
		},
	}

	// Add stats sink to push metrics to OTEL collector
	if g.otel.Enabled && g.otel.Endpoint != "" {
		config["stats_sinks"] = g.buildStatsSinks()
	}

	return config
}

// buildListeners builds the listeners configuration - one per unique port.
func (g *EnvoyConfigGenerator) buildListeners(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) []interface{} {
	if len(ingresses) == 0 {
		return []interface{}{}
	}

	// Group rules by port
	portToFilterChains := g.buildFilterChainsByPort(ctx, ingresses, ipResolver)
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
func (g *EnvoyConfigGenerator) buildFilterChainsByPort(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) map[int][]interface{} {
	log := logger.FromContext(ctx)

	// Group virtual hosts by port
	portToVirtualHosts := make(map[int][]interface{})

	for _, ingress := range ingresses {
		for _, rule := range ingress.Rules {
			// Resolve instance IP - skip rules where we can't resolve
			_, err := ipResolver(rule.Target.Instance)
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
func (g *EnvoyConfigGenerator) buildClusters(ctx context.Context, ingresses []Ingress, ipResolver func(instance string) (string, error)) []interface{} {
	log := logger.FromContext(ctx)

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
				log.WarnContext(ctx, "skipping cluster: cannot resolve instance IP",
					"ingress_id", ingress.ID,
					"instance", rule.Target.Instance,
					"error", err)
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

// otelCollectorClusterName is the cluster name for the OTEL collector.
const otelCollectorClusterName = "opentelemetry_collector"

// buildOTELCollectorCluster builds the cluster configuration for the OTEL collector.
func (g *EnvoyConfigGenerator) buildOTELCollectorCluster() map[string]interface{} {
	// Parse endpoint (host:port)
	host, port := parseEndpoint(g.otel.Endpoint)

	return map[string]interface{}{
		"name":            otelCollectorClusterName,
		"type":            "STRICT_DNS",
		"connect_timeout": "5s",
		"lb_policy":       "ROUND_ROBIN",
		"typed_extension_protocol_options": map[string]interface{}{
			"envoy.extensions.upstreams.http.v3.HttpProtocolOptions": map[string]interface{}{
				"@type": "type.googleapis.com/envoy.extensions.upstreams.http.v3.HttpProtocolOptions",
				"explicit_http_config": map[string]interface{}{
					"http2_protocol_options": map[string]interface{}{},
				},
			},
		},
		"load_assignment": map[string]interface{}{
			"cluster_name": otelCollectorClusterName,
			"endpoints": []interface{}{
				map[string]interface{}{
					"lb_endpoints": []interface{}{
						map[string]interface{}{
							"endpoint": map[string]interface{}{
								"address": map[string]interface{}{
									"socket_address": map[string]interface{}{
										"address":    host,
										"port_value": port,
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

// buildStatsSinks builds the stats sinks configuration for metrics export to OTEL.
func (g *EnvoyConfigGenerator) buildStatsSinks() []interface{} {
	serviceName := g.otel.ServiceName
	if serviceName == "" {
		serviceName = "hypeman-envoy"
	}

	// Build resource attributes for metrics
	resourceAttrs := map[string]interface{}{
		"service.name": serviceName,
	}
	if g.otel.Environment != "" {
		resourceAttrs["deployment.environment.name"] = g.otel.Environment
	}
	if g.otel.ServiceInstanceID != "" {
		resourceAttrs["service.instance.id"] = g.otel.ServiceInstanceID
	}

	return []interface{}{
		map[string]interface{}{
			"name": "envoy.stat_sinks.open_telemetry",
			"typed_config": map[string]interface{}{
				"@type": "type.googleapis.com/envoy.extensions.stat_sinks.open_telemetry.v3.SinkConfig",
				"grpc_service": map[string]interface{}{
					"envoy_grpc": map[string]interface{}{
						"cluster_name": otelCollectorClusterName,
					},
					"timeout": "5s",
				},
				"emit_tags_as_attributes": true,
				"prefix":                  "envoy",
			},
		},
	}
}

// parseEndpoint parses a host:port string. Defaults to port 4317 if not specified.
func parseEndpoint(endpoint string) (string, int) {
	parts := strings.Split(endpoint, ":")
	if len(parts) == 2 {
		port := 4317
		if p, err := strconv.Atoi(parts[1]); err == nil {
			port = p
		}
		return parts[0], port
	}
	// Default OTLP gRPC port
	return endpoint, 4317
}
