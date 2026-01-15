package config

import (
	"fmt"
	"os"
	"runtime/debug"
	"strconv"

	"github.com/joho/godotenv"
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
	Port                string
	DataDir             string
	BridgeName          string
	SubnetCIDR          string
	SubnetGateway       string
	UplinkInterface     string
	JwtSecret           string
	DNSServer           string
	MaxConcurrentBuilds int
	MaxOverlaySize      string
	LogMaxSize          string
	LogMaxFiles         int
	LogRotateInterval   string

	// Resource limits - per instance
	MaxVcpusPerInstance  int    // Max vCPUs for a single VM (0 = unlimited)
	MaxMemoryPerInstance string // Max memory for a single VM (0 = unlimited)

	// Resource limits - aggregate
	MaxTotalVcpus         int    // Aggregate vCPU limit across all instances (0 = unlimited)
	MaxTotalMemory        string // Aggregate memory limit across all instances (0 = unlimited)
	MaxTotalVolumeStorage string // Total volume storage limit (0 = unlimited)

	// OpenTelemetry configuration
	OtelEnabled           bool   // Enable OpenTelemetry
	OtelEndpoint          string // OTLP endpoint (gRPC)
	OtelServiceName       string // Service name for tracing
	OtelServiceInstanceID string // Service instance ID (default: hostname)
	OtelInsecure          bool   // Disable TLS for OTLP
	Version               string // Application version for telemetry
	Env                   string // Deployment environment (e.g., dev, staging, prod)

	// Logging configuration
	LogLevel string // Default log level (debug, info, warn, error)

	// Caddy / Ingress configuration
	CaddyListenAddress  string // Address for Caddy to listen on
	CaddyAdminAddress   string // Address for Caddy admin API
	CaddyAdminPort      int    // Port for Caddy admin API
	InternalDNSPort     int    // Port for internal DNS server (used for dynamic upstreams)
	CaddyStopOnShutdown bool   // Stop Caddy when hypeman shuts down

	// ACME / TLS configuration
	AcmeEmail             string // ACME account email (required for TLS ingresses)
	AcmeDnsProvider       string // DNS provider: "cloudflare"
	AcmeCA                string // ACME CA URL (empty = Let's Encrypt production)
	DnsPropagationTimeout string // Max time to wait for DNS propagation (e.g., "2m")
	DnsResolvers          string // Comma-separated DNS resolvers for propagation checking
	TlsAllowedDomains     string // Comma-separated list of allowed domain patterns for TLS (e.g., "*.example.com,api.example.com")

	// Cloudflare configuration (if AcmeDnsProvider=cloudflare)
	CloudflareApiToken string // Cloudflare API token

	// Build system configuration
	MaxConcurrentSourceBuilds int    // Max concurrent source-to-image builds
	BuilderImage              string // OCI image for builder VMs
	RegistryURL               string // URL of registry for built images
	BuildTimeout              int    // Default build timeout in seconds
	BuildSecretsDir           string // Directory containing build secrets (optional)

	// Hypervisor configuration
	DefaultHypervisor string // Default hypervisor type: "cloud-hypervisor" or "qemu"

	// Oversubscription ratios (1.0 = no oversubscription, 2.0 = 2x oversubscription)
	OversubCPU     float64 // CPU oversubscription ratio
	OversubMemory  float64 // Memory oversubscription ratio
	OversubDisk    float64 // Disk oversubscription ratio
	OversubNetwork float64 // Network oversubscription ratio
	OversubDiskIO  float64 // Disk I/O oversubscription ratio

	// Network rate limiting
	UploadBurstMultiplier   int // Multiplier for upload burst ceiling vs guaranteed rate (default: 4)
	DownloadBurstMultiplier int // Multiplier for download burst bucket vs rate (default: 4)

	// Resource capacity limits (empty = auto-detect from host)
	DiskLimit       string  // Hard disk limit for DataDir, e.g. "500GB"
	NetworkLimit    string  // Hard network limit, e.g. "10Gbps" (empty = detect from uplink speed)
	DiskIOLimit     string  // Hard disk I/O limit, e.g. "500MB/s" (empty = auto-detect from disk type)
	MaxImageStorage float64 // Max image storage as fraction of disk (0.2 = 20%), counts OCI cache + rootfs
}

// Load loads configuration from environment variables
// Automatically loads .env file if present
func Load() *Config {
	// Try to load .env file (fail silently if not present)
	_ = godotenv.Load()

	cfg := &Config{
		Port:                getEnv("PORT", "8080"),
		DataDir:             getEnv("DATA_DIR", "/var/lib/hypeman"),
		BridgeName:          getEnv("BRIDGE_NAME", "vmbr0"),
		SubnetCIDR:          getEnv("SUBNET_CIDR", "10.100.0.0/16"),
		SubnetGateway:       getEnv("SUBNET_GATEWAY", ""),   // empty = derived as first IP from subnet
		UplinkInterface:     getEnv("UPLINK_INTERFACE", ""), // empty = auto-detect from default route
		JwtSecret:           getEnv("JWT_SECRET", ""),
		DNSServer:           getEnv("DNS_SERVER", "1.1.1.1"),
		MaxConcurrentBuilds: getEnvInt("MAX_CONCURRENT_BUILDS", 1),
		MaxOverlaySize:      getEnv("MAX_OVERLAY_SIZE", "100GB"),
		LogMaxSize:          getEnv("LOG_MAX_SIZE", "50MB"),
		LogMaxFiles:         getEnvInt("LOG_MAX_FILES", 1),
		LogRotateInterval:   getEnv("LOG_ROTATE_INTERVAL", "5m"),

		// Resource limits - per instance (0 = unlimited)
		MaxVcpusPerInstance:  getEnvInt("MAX_VCPUS_PER_INSTANCE", 16),
		MaxMemoryPerInstance: getEnv("MAX_MEMORY_PER_INSTANCE", "32GB"),

		// Resource limits - aggregate (0 or empty = unlimited)
		MaxTotalVcpus:         getEnvInt("MAX_TOTAL_VCPUS", 0),
		MaxTotalMemory:        getEnv("MAX_TOTAL_MEMORY", ""),
		MaxTotalVolumeStorage: getEnv("MAX_TOTAL_VOLUME_STORAGE", ""),

		// OpenTelemetry configuration
		OtelEnabled:           getEnvBool("OTEL_ENABLED", false),
		OtelEndpoint:          getEnv("OTEL_ENDPOINT", "127.0.0.1:4317"),
		OtelServiceName:       getEnv("OTEL_SERVICE_NAME", "hypeman"),
		OtelServiceInstanceID: getEnv("OTEL_SERVICE_INSTANCE_ID", getHostname()),
		OtelInsecure:          getEnvBool("OTEL_INSECURE", true),
		Version:               getEnv("VERSION", getBuildVersion()),
		Env:                   getEnv("ENV", "unset"),

		// Logging configuration
		LogLevel: getEnv("LOG_LEVEL", "info"),

		// Caddy / Ingress configuration
		CaddyListenAddress: getEnv("CADDY_LISTEN_ADDRESS", "0.0.0.0"),
		CaddyAdminAddress:  getEnv("CADDY_ADMIN_ADDRESS", "127.0.0.1"),
		CaddyAdminPort:     getEnvInt("CADDY_ADMIN_PORT", 0),  // 0 = random port to prevent conflicts on shared dev machines
		InternalDNSPort:    getEnvInt("INTERNAL_DNS_PORT", 0), // 0 = random port; used for dynamic upstream resolution
		// Set to false if you're likely to frequently update hypeman
		CaddyStopOnShutdown: getEnvBool("CADDY_STOP_ON_SHUTDOWN", true),

		// ACME / TLS configuration
		AcmeEmail:             getEnv("ACME_EMAIL", ""),
		AcmeDnsProvider:       getEnv("ACME_DNS_PROVIDER", ""),
		AcmeCA:                getEnv("ACME_CA", ""),
		DnsPropagationTimeout: getEnv("DNS_PROPAGATION_TIMEOUT", ""),
		DnsResolvers:          getEnv("DNS_RESOLVERS", ""),
		TlsAllowedDomains:     getEnv("TLS_ALLOWED_DOMAINS", ""), // Empty = no TLS domains allowed

		// Cloudflare configuration
		CloudflareApiToken: getEnv("CLOUDFLARE_API_TOKEN", ""),

		// Build system configuration
		MaxConcurrentSourceBuilds: getEnvInt("MAX_CONCURRENT_SOURCE_BUILDS", 2),
		BuilderImage:              getEnv("BUILDER_IMAGE", "hypeman/builder:latest"),
		RegistryURL:               getEnv("REGISTRY_URL", "localhost:8080"),
		BuildTimeout:              getEnvInt("BUILD_TIMEOUT", 600),
		BuildSecretsDir:           getEnv("BUILD_SECRETS_DIR", ""), // Optional: path to directory with build secrets

		// Hypervisor configuration
		DefaultHypervisor: getEnv("DEFAULT_HYPERVISOR", "cloud-hypervisor"),

		// Oversubscription ratios (1.0 = no oversubscription)
		OversubCPU:     getEnvFloat("OVERSUB_CPU", 4.0),
		OversubMemory:  getEnvFloat("OVERSUB_MEMORY", 1.0),
		OversubDisk:    getEnvFloat("OVERSUB_DISK", 1.0),
		OversubNetwork: getEnvFloat("OVERSUB_NETWORK", 2.0),
		OversubDiskIO:  getEnvFloat("OVERSUB_DISK_IO", 2.0),

		// Network rate limiting
		UploadBurstMultiplier:   getEnvInt("UPLOAD_BURST_MULTIPLIER", 4),
		DownloadBurstMultiplier: getEnvInt("DOWNLOAD_BURST_MULTIPLIER", 4),

		// Resource capacity limits (empty = auto-detect)
		DiskLimit:       getEnv("DISK_LIMIT", ""),
		NetworkLimit:    getEnv("NETWORK_LIMIT", ""),
		DiskIOLimit:     getEnv("DISK_IO_LIMIT", ""),
		MaxImageStorage: getEnvFloat("MAX_IMAGE_STORAGE", 0.2), // 20% of disk by default
	}

	return cfg
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

func getEnvInt(key string, defaultValue int) int {
	if value := os.Getenv(key); value != "" {
		if intVal, err := strconv.Atoi(value); err == nil {
			return intVal
		}
	}
	return defaultValue
}

func getEnvBool(key string, defaultValue bool) bool {
	if value := os.Getenv(key); value != "" {
		if boolVal, err := strconv.ParseBool(value); err == nil {
			return boolVal
		}
	}
	return defaultValue
}

func getEnvFloat(key string, defaultValue float64) float64 {
	if value := os.Getenv(key); value != "" {
		if floatVal, err := strconv.ParseFloat(value, 64); err == nil {
			return floatVal
		}
	}
	return defaultValue
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
