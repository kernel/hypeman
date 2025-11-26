package config

import (
	"os"
	"strconv"

	"github.com/joho/godotenv"
)

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
		SubnetGateway:       getEnv("SUBNET_GATEWAY", ""), // empty = derived as first IP from subnet
		UplinkInterface:     getEnv("UPLINK_INTERFACE", ""), // empty = auto-detect from default route
		JwtSecret:           getEnv("JWT_SECRET", ""),
		DNSServer:           getEnv("DNS_SERVER", "1.1.1.1"),
		MaxConcurrentBuilds: getEnvInt("MAX_CONCURRENT_BUILDS", 1),
		MaxOverlaySize:      getEnv("MAX_OVERLAY_SIZE", "100GB"),
		LogMaxSize:          getEnv("LOG_MAX_SIZE", "50MB"),
		LogMaxFiles:         getEnvInt("LOG_MAX_FILES", 1),
		LogRotateInterval:   getEnv("LOG_ROTATE_INTERVAL", "5m"),
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

