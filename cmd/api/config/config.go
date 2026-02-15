package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/knadh/koanf/parsers/yaml"
	"github.com/knadh/koanf/providers/env"
	"github.com/knadh/koanf/providers/file"
	"github.com/knadh/koanf/providers/structs"
	"github.com/knadh/koanf/v2"
)

func getHostname() string {
	if h, err := os.Hostname(); err == nil {
		return h
	}
	return "unknown"
}

// getBuildVersion extracts version info from Go's embedded build info.
// Returns git short hash + "-dirty" suffix if uncommitted changes, or "unknown" if unavailable.
func getBuildVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}

	var revision string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	if revision == "" {
		return "unknown"
	}

	// Use short hash (8 chars)
	if len(revision) > 8 {
		revision = revision[:8]
	}
	if dirty {
		revision += "-dirty"
	}
	return revision
}

type Config struct {
	Port                string `koanf:"port"`
	DataDir             string `koanf:"data_dir"`
	BridgeName          string `koanf:"bridge_name"`
	SubnetCIDR          string `koanf:"subnet_cidr"`
	SubnetGateway       string `koanf:"subnet_gateway"`
	UplinkInterface     string `koanf:"uplink_interface"`
	JwtSecret           string `koanf:"jwt_secret"`
	DNSServer           string `koanf:"dns_server"`
	MaxConcurrentBuilds int    `koanf:"max_concurrent_builds"`
	MaxOverlaySize      string `koanf:"max_overlay_size"`
	LogMaxSize          string `koanf:"log_max_size"`
	LogMaxFiles         int    `koanf:"log_max_files"`
	LogRotateInterval   string `koanf:"log_rotate_interval"`

	// Resource limits - per instance
	MaxVcpusPerInstance  int    `koanf:"max_vcpus_per_instance"`  // Max vCPUs for a single VM (0 = unlimited)
	MaxMemoryPerInstance string `koanf:"max_memory_per_instance"` // Max memory for a single VM (0 = unlimited)

	// Resource limits - aggregate
	// Note: CPU/memory aggregate limits are now handled via oversubscription ratios (OVERSUB_CPU, OVERSUB_MEMORY)
	MaxTotalVolumeStorage string `koanf:"max_total_volume_storage"` // Total volume storage limit (0 = unlimited)

	// OpenTelemetry configuration
	OtelEnabled           bool   `koanf:"otel_enabled"`             // Enable OpenTelemetry
	OtelEndpoint          string `koanf:"otel_endpoint"`            // OTLP endpoint (gRPC)
	OtelServiceName       string `koanf:"otel_service_name"`        // Service name for tracing
	OtelServiceInstanceID string `koanf:"otel_service_instance_id"` // Service instance ID (default: hostname)
	OtelInsecure          bool   `koanf:"otel_insecure"`            // Disable TLS for OTLP
	Version               string `koanf:"version"`                  // Application version for telemetry
	Env                   string `koanf:"env"`                      // Deployment environment (e.g., dev, staging, prod)

	// Logging configuration
	LogLevel string `koanf:"log_level"` // Default log level (debug, info, warn, error)

	// Caddy / Ingress configuration
	CaddyListenAddress  string `koanf:"caddy_listen_address"`   // Address for Caddy to listen on
	CaddyAdminAddress   string `koanf:"caddy_admin_address"`    // Address for Caddy admin API
	CaddyAdminPort      int    `koanf:"caddy_admin_port"`       // Port for Caddy admin API
	InternalDNSPort     int    `koanf:"internal_dns_port"`      // Port for internal DNS server (used for dynamic upstreams)
	CaddyStopOnShutdown bool   `koanf:"caddy_stop_on_shutdown"` // Stop Caddy when hypeman shuts down

	// ACME / TLS configuration
	AcmeEmail             string `koanf:"acme_email"`              // ACME account email (required for TLS ingresses)
	AcmeDnsProvider       string `koanf:"acme_dns_provider"`       // DNS provider: "cloudflare"
	AcmeCA                string `koanf:"acme_ca"`                 // ACME CA URL (empty = Let's Encrypt production)
	DnsPropagationTimeout string `koanf:"dns_propagation_timeout"` // Max time to wait for DNS propagation (e.g., "2m")
	DnsResolvers          string `koanf:"dns_resolvers"`           // Comma-separated DNS resolvers for propagation checking
	TlsAllowedDomains     string `koanf:"tls_allowed_domains"`     // Comma-separated list of allowed domain patterns for TLS (e.g., "*.example.com,api.example.com")

	// Cloudflare configuration (if AcmeDnsProvider=cloudflare)
	CloudflareApiToken string `koanf:"cloudflare_api_token"` // Cloudflare API token

	// API ingress configuration - exposes Hypeman API via Caddy
	ApiHostname     string `koanf:"api_hostname"`      // Hostname for API access (e.g., hypeman.hostname.kernel.sh). Empty = disabled.
	ApiTLS          bool   `koanf:"api_tls"`           // Enable TLS for API hostname
	ApiRedirectHTTP bool   `koanf:"api_redirect_http"` // Redirect HTTP to HTTPS for API hostname

	// Build system configuration
	MaxConcurrentSourceBuilds int    `koanf:"max_concurrent_source_builds"` // Max concurrent source-to-image builds
	BuilderImage              string `koanf:"builder_image"`                // OCI image for builder VMs
	RegistryURL               string `koanf:"registry_url"`                 // URL of registry for built images
	RegistryInsecure          bool   `koanf:"registry_insecure"`            // Skip TLS verification for registry (for self-signed certs)
	RegistryCACertFile        string `koanf:"registry_ca_cert_file"`        // Path to CA certificate file for registry TLS verification
	BuildTimeout              int    `koanf:"build_timeout"`                // Default build timeout in seconds
	BuildSecretsDir           string `koanf:"build_secrets_dir"`            // Directory containing build secrets (optional)
	DockerSocket              string `koanf:"docker_socket"`                // Path to Docker socket (for building builder image)

	// Hypervisor configuration
	DefaultHypervisor string `koanf:"default_hypervisor"` // Default hypervisor type: "cloud-hypervisor" or "qemu"

	// GPU configuration
	GPUProfileCacheTTL string `koanf:"gpu_profile_cache_ttl"` // TTL for GPU profile metadata cache (e.g., "30m")

	// Oversubscription ratios (1.0 = no oversubscription, 2.0 = 2x oversubscription)
	OversubCPU     float64 `koanf:"oversub_cpu"`      // CPU oversubscription ratio
	OversubMemory  float64 `koanf:"oversub_memory"`   // Memory oversubscription ratio
	OversubDisk    float64 `koanf:"oversub_disk"`     // Disk oversubscription ratio
	OversubNetwork float64 `koanf:"oversub_network"`  // Network oversubscription ratio
	OversubDiskIO  float64 `koanf:"oversub_disk_io"`  // Disk I/O oversubscription ratio

	// Network rate limiting
	UploadBurstMultiplier   int `koanf:"upload_burst_multiplier"`   // Multiplier for upload burst ceiling vs guaranteed rate (default: 4)
	DownloadBurstMultiplier int `koanf:"download_burst_multiplier"` // Multiplier for download burst bucket vs rate (default: 4)

	// Resource capacity limits (empty = auto-detect from host)
	DiskLimit       string  `koanf:"disk_limit"`        // Hard disk limit for DataDir, e.g. "500GB"
	NetworkLimit    string  `koanf:"network_limit"`     // Hard network limit, e.g. "10Gbps" (empty = detect from uplink speed)
	DiskIOLimit     string  `koanf:"disk_io_limit"`     // Hard disk I/O limit, e.g. "500MB/s" (empty = auto-detect from disk type)
	MaxImageStorage float64 `koanf:"max_image_storage"` // Max image storage as fraction of disk (0.2 = 20%), counts OCI cache + rootfs
}

// GetDefaultConfigPaths returns the default config file paths to search.
// Returns paths in order of precedence (first found wins).
func GetDefaultConfigPaths() []string {
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return []string{
			filepath.Join(home, ".config", "hypeman", "config.yaml"),
		}
	}
	// Linux: check /etc first, then user config
	return []string{
		"/etc/hypeman/config.yaml",
		filepath.Join(home, ".config", "hypeman", "config.yaml"),
	}
}

// defaultConfig returns a Config struct with all default values set.
func defaultConfig() *Config {
	return &Config{
		Port:                "8080",
		DataDir:             "/var/lib/hypeman",
		BridgeName:          "vmbr0",
		SubnetCIDR:          "10.100.0.0/16",
		SubnetGateway:       "", // empty = derived as first IP from subnet
		UplinkInterface:     "", // empty = auto-detect from default route
		JwtSecret:           "",
		DNSServer:           "1.1.1.1",
		MaxConcurrentBuilds: 1,
		MaxOverlaySize:      "100GB",
		LogMaxSize:          "50MB",
		LogMaxFiles:         1,
		LogRotateInterval:   "5m",

		// Resource limits - per instance (0 = unlimited)
		MaxVcpusPerInstance:  16,
		MaxMemoryPerInstance: "32GB",

		// Resource limits - aggregate
		MaxTotalVolumeStorage: "",

		// OpenTelemetry configuration
		OtelEnabled:           false,
		OtelEndpoint:          "127.0.0.1:4317",
		OtelServiceName:       "hypeman",
		OtelServiceInstanceID: getHostname(),
		OtelInsecure:          true,
		Version:               getBuildVersion(),
		Env:                   "unset",

		// Logging configuration
		LogLevel: "info",

		// Caddy / Ingress configuration
		CaddyListenAddress:  "0.0.0.0",
		CaddyAdminAddress:   "127.0.0.1",
		CaddyAdminPort:      0, // 0 = random port to prevent conflicts on shared dev machines
		InternalDNSPort:     0, // 0 = random port; used for dynamic upstream resolution
		CaddyStopOnShutdown: true,

		// ACME / TLS configuration
		AcmeEmail:             "",
		AcmeDnsProvider:       "",
		AcmeCA:                "",
		DnsPropagationTimeout: "",
		DnsResolvers:          "",
		TlsAllowedDomains:     "",

		// Cloudflare configuration
		CloudflareApiToken: "",

		// API ingress configuration
		ApiHostname:     "",   // Empty = disabled
		ApiTLS:          true, // Default to TLS enabled
		ApiRedirectHTTP: true,

		// Build system configuration
		MaxConcurrentSourceBuilds: 2,
		BuilderImage:              "hypeman/builder:latest",
		RegistryURL:               "localhost:8080",
		RegistryInsecure:          false,
		RegistryCACertFile:        "",
		BuildTimeout:              600,
		BuildSecretsDir:           "",
		DockerSocket:              "/var/run/docker.sock",

		// Hypervisor configuration
		DefaultHypervisor: "cloud-hypervisor",

		// GPU configuration
		GPUProfileCacheTTL: "30m",

		// Oversubscription ratios (1.0 = no oversubscription)
		OversubCPU:     4.0,
		OversubMemory:  1.0,
		OversubDisk:    1.0,
		OversubNetwork: 2.0,
		OversubDiskIO:  2.0,

		// Network rate limiting
		UploadBurstMultiplier:   4,
		DownloadBurstMultiplier: 4,

		// Resource capacity limits (empty = auto-detect)
		DiskLimit:       "",
		NetworkLimit:    "",
		DiskIOLimit:     "",
		MaxImageStorage: 0.2, // 20% of disk by default
	}
}

// Load loads configuration with the following precedence (highest to lowest):
// 1. Environment variables (e.g., PORT, DATA_DIR, JWT_SECRET)
// 2. YAML config file (if found)
// 3. Default values
//
// The configPath parameter specifies an explicit config file path.
// If empty, searches default locations based on OS.
// Returns an error if an explicitly provided configPath cannot be loaded.
func Load(configPath string) (*Config, error) {
	k := koanf.New(".")

	// 1. Load defaults first
	defaults := defaultConfig()
	if err := k.Load(structs.Provider(defaults, "koanf"), nil); err != nil {
		return nil, fmt.Errorf("failed to load default config: %w", err)
	}

	// 2. Load from YAML config file
	explicitPath := configPath != ""
	if !explicitPath {
		// Search default paths (best-effort, file may not exist)
		for _, path := range GetDefaultConfigPaths() {
			if _, err := os.Stat(path); err == nil {
				configPath = path
				break
			}
		}
	}
	if configPath != "" {
		if err := k.Load(file.Provider(configPath), yaml.Parser()); err != nil {
			if explicitPath {
				// Explicit path must be loadable
				return nil, fmt.Errorf("failed to load config from %s: %w", configPath, err)
			}
			// Auto-discovered path failed — continue with defaults + env
		}
	}

	// 3. Overlay environment variables (highest precedence)
	// Environment variables use SCREAMING_SNAKE_CASE (e.g., JWT_SECRET, DATA_DIR)
	// They map to flat snake_case koanf keys (e.g., jwt_secret, data_dir)
	// Note: delim must be "" to prevent unflattening (e.g., DATA_DIR → data.dir)
	// Using ProviderWithValue to skip empty env vars, preserving default/YAML values.
	envProvider := env.ProviderWithValue("", "", func(key string, value string) (string, interface{}) {
		if value == "" {
			return "", nil
		}
		return strings.ToLower(key), value
	})
	_ = k.Load(envProvider, nil)

	// 4. Unmarshal to Config struct
	var cfg Config
	if err := k.Unmarshal("", &cfg); err != nil {
		return nil, fmt.Errorf("failed to unmarshal config: %w", err)
	}

	return &cfg, nil
}

// Validate checks configuration values for correctness.
// Returns an error if any configuration value is invalid.
func (c *Config) Validate() error {
	// Validate oversubscription ratios are positive
	if c.OversubCPU <= 0 {
		return fmt.Errorf("OVERSUB_CPU must be positive, got %v", c.OversubCPU)
	}
	if c.OversubMemory <= 0 {
		return fmt.Errorf("OVERSUB_MEMORY must be positive, got %v", c.OversubMemory)
	}
	if c.OversubDisk <= 0 {
		return fmt.Errorf("OVERSUB_DISK must be positive, got %v", c.OversubDisk)
	}
	if c.OversubNetwork <= 0 {
		return fmt.Errorf("OVERSUB_NETWORK must be positive, got %v", c.OversubNetwork)
	}
	if c.OversubDiskIO <= 0 {
		return fmt.Errorf("OVERSUB_DISK_IO must be positive, got %v", c.OversubDiskIO)
	}
	if c.UploadBurstMultiplier < 1 {
		return fmt.Errorf("UPLOAD_BURST_MULTIPLIER must be >= 1, got %v", c.UploadBurstMultiplier)
	}
	if c.DownloadBurstMultiplier < 1 {
		return fmt.Errorf("DOWNLOAD_BURST_MULTIPLIER must be >= 1, got %v", c.DownloadBurstMultiplier)
	}
	return nil
}
