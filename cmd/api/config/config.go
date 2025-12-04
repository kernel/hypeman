package config

import (
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

	// Envoy / Ingress configuration
	EnvoyListenAddress string // Address for Envoy to listen on (default: 0.0.0.0)
	EnvoyListenPort    int    // Port for Envoy to listen on (default: 80)
	EnvoyAdminAddress  string // Address for Envoy admin API (default: 127.0.0.1)
	EnvoyAdminPort     int    // Port for Envoy admin API (default: 9901)
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

		// Envoy / Ingress configuration
		EnvoyListenAddress: getEnv("ENVOY_LISTEN_ADDRESS", "0.0.0.0"),
		EnvoyListenPort:    getEnvInt("ENVOY_LISTEN_PORT", 80),
		EnvoyAdminAddress:  getEnv("ENVOY_ADMIN_ADDRESS", "127.0.0.1"),
		EnvoyAdminPort:     getEnvInt("ENVOY_ADMIN_PORT", 9901),
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
