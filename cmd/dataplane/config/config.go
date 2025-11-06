package config

import (
	"os"
)

type Config struct {
	Port             string
	DataDir          string
	BridgeName       string
	SubnetCIDR       string
	SubnetGateway    string
	ContainerdSocket string
	JwtSecret        string
	DNSServer        string
}

func Load() *Config {
	return &Config{
		Port:             getEnv("PORT", "8080"),
		DataDir:          getEnv("DATA_DIR", "/var/lib/cloud-hypervisor-dataplane"),
		BridgeName:       getEnv("BRIDGE_NAME", "vmbr0"),
		SubnetCIDR:       getEnv("SUBNET_CIDR", "192.168.100.0/24"),
		SubnetGateway:    getEnv("SUBNET_GATEWAY", "192.168.100.1"),
		ContainerdSocket: getEnv("CONTAINERD_SOCKET", "/run/containerd/containerd.sock"),
		JwtSecret:        getEnv("JWT_SECRET", ""),
		DNSServer:        getEnv("DNS_SERVER", "1.1.1.1"),
	}
}

func getEnv(key, defaultValue string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return defaultValue
}

