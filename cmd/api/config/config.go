package config

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"time"

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

// NetworkConfig holds network bridge and interface settings.
type NetworkConfig struct {
	BridgeName              string `koanf:"bridge_name"`
	SubnetCIDR              string `koanf:"subnet_cidr"`
	SubnetGateway           string `koanf:"subnet_gateway"`
	UplinkInterface         string `koanf:"uplink_interface"`
	DNSServer               string `koanf:"dns_server"`
	UploadBurstMultiplier   int    `koanf:"upload_burst_multiplier"`
	DownloadBurstMultiplier int    `koanf:"download_burst_multiplier"`
}

// CaddyConfig holds Caddy reverse-proxy / ingress settings.
type CaddyConfig struct {
	ListenAddress   string `koanf:"listen_address"`
	AdminAddress    string `koanf:"admin_address"`
	AdminPort       int    `koanf:"admin_port"`
	InternalDNSPort int    `koanf:"internal_dns_port"`
	StopOnShutdown  bool   `koanf:"stop_on_shutdown"`
}

// ACMEConfig holds ACME / TLS certificate settings.
type ACMEConfig struct {
	Email                 string `koanf:"email"`
	DNSProvider           string `koanf:"dns_provider"`
	CA                    string `koanf:"ca"`
	DNSPropagationTimeout string `koanf:"dns_propagation_timeout"`
	DNSResolvers          string `koanf:"dns_resolvers"`
	AllowedDomains        string `koanf:"allowed_domains"`
	CloudflareAPIToken    string `koanf:"cloudflare_api_token"`
}

// APIConfig holds API ingress settings (exposes Hypeman API via Caddy).
type APIConfig struct {
	Hostname     string `koanf:"hostname"`
	TLS          bool   `koanf:"tls"`
	RedirectHTTP bool   `koanf:"redirect_http"`
}

// MetricsConfig holds metrics endpoint settings.
type MetricsConfig struct {
	ListenAddress string `koanf:"listen_address"`
	Port          int    `koanf:"port"`
	VMLabelBudget int    `koanf:"vm_label_budget"`
}

// OtelConfig holds OpenTelemetry settings.
type OtelConfig struct {
	Enabled              bool   `koanf:"enabled"`
	Endpoint             string `koanf:"endpoint"`
	ServiceName          string `koanf:"service_name"`
	ServiceInstanceID    string `koanf:"service_instance_id"`
	Insecure             bool   `koanf:"insecure"`
	MetricExportInterval string `koanf:"metric_export_interval"`
}

// LoggingConfig holds log rotation and level settings.
type LoggingConfig struct {
	Level          string `koanf:"level"`
	MaxSize        string `koanf:"max_size"`
	MaxFiles       int    `koanf:"max_files"`
	RotateInterval string `koanf:"rotate_interval"`
}

// BuildConfig holds source-to-image build system settings.
type BuildConfig struct {
	MaxConcurrentSourceBuilds int    `koanf:"max_concurrent_source_builds"`
	BuilderImage              string `koanf:"builder_image"`
	Timeout                   int    `koanf:"timeout"`
	SecretsDir                string `koanf:"secrets_dir"`
	DockerSocket              string `koanf:"docker_socket"`
}

// RegistryConfig holds OCI registry settings.
type RegistryConfig struct {
	URL        string `koanf:"url"`
	Insecure   bool   `koanf:"insecure"`
	CACertFile string `koanf:"ca_cert_file"`
}

// LimitsConfig holds per-instance and aggregate resource limits.
type LimitsConfig struct {
	MaxVcpusPerInstance   int     `koanf:"max_vcpus_per_instance"`
	MaxMemoryPerInstance  string  `koanf:"max_memory_per_instance"`
	MaxTotalVolumeStorage string  `koanf:"max_total_volume_storage"`
	MaxConcurrentBuilds   int     `koanf:"max_concurrent_builds"`
	MaxOverlaySize        string  `koanf:"max_overlay_size"`
	MaxImageStorage       float64 `koanf:"max_image_storage"`
}

// OversubscriptionConfig holds oversubscription ratios (1.0 = no oversubscription).
type OversubscriptionConfig struct {
	CPU     float64 `koanf:"cpu"`
	Memory  float64 `koanf:"memory"`
	Disk    float64 `koanf:"disk"`
	Network float64 `koanf:"network"`
	DiskIO  float64 `koanf:"disk_io"`
}

// CapacityConfig holds hard resource capacity limits (empty = auto-detect from host).
type CapacityConfig struct {
	Disk    string `koanf:"disk"`
	Network string `koanf:"network"`
	DiskIO  string `koanf:"disk_io"`
}

// HypervisorConfig holds hypervisor settings.
type HypervisorConfig struct {
	Default               string                 `koanf:"default"`
	FirecrackerBinaryPath string                 `koanf:"firecracker_binary_path"`
	Memory                HypervisorMemoryConfig `koanf:"memory"`
}

// HypervisorMemoryConfig holds guest memory management settings.
type HypervisorMemoryConfig struct {
	Enabled            bool   `koanf:"enabled"`
	KernelPageInitMode string `koanf:"kernel_page_init_mode"`
	ReclaimEnabled     bool   `koanf:"reclaim_enabled"`
	VZBalloonRequired  bool   `koanf:"vz_balloon_required"`
}

// GPUConfig holds GPU-related settings.
type GPUConfig struct {
	ProfileCacheTTL string `koanf:"profile_cache_ttl"`
}

// Config is the top-level Hypeman server configuration.
type Config struct {
	Port      string `koanf:"port"`
	DataDir   string `koanf:"data_dir"`
	JwtSecret string `koanf:"jwt_secret"`
	Env       string `koanf:"env"`
	Version   string `koanf:"version"`

	Network          NetworkConfig          `koanf:"network"`
	Caddy            CaddyConfig            `koanf:"caddy"`
	ACME             ACMEConfig             `koanf:"acme"`
	API              APIConfig              `koanf:"api"`
	Metrics          MetricsConfig          `koanf:"metrics"`
	Otel             OtelConfig             `koanf:"otel"`
	Logging          LoggingConfig          `koanf:"logging"`
	Build            BuildConfig            `koanf:"build"`
	Registry         RegistryConfig         `koanf:"registry"`
	Limits           LimitsConfig           `koanf:"limits"`
	Oversubscription OversubscriptionConfig `koanf:"oversubscription"`
	Capacity         CapacityConfig         `koanf:"capacity"`
	Hypervisor       HypervisorConfig       `koanf:"hypervisor"`
	GPU              GPUConfig              `koanf:"gpu"`
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
		Port:      "8080",
		DataDir:   "/var/lib/hypeman",
		JwtSecret: "",
		Env:       "unset",
		Version:   getBuildVersion(),

		Network: NetworkConfig{
			BridgeName:              "vmbr0",
			SubnetCIDR:              "10.100.0.0/16",
			SubnetGateway:           "",
			UplinkInterface:         "",
			DNSServer:               "1.1.1.1",
			UploadBurstMultiplier:   4,
			DownloadBurstMultiplier: 4,
		},

		Caddy: CaddyConfig{
			ListenAddress:   "0.0.0.0",
			AdminAddress:    "127.0.0.1",
			AdminPort:       0,
			InternalDNSPort: 0,
			StopOnShutdown:  true,
		},

		ACME: ACMEConfig{
			Email:                 "",
			DNSProvider:           "",
			CA:                    "",
			DNSPropagationTimeout: "",
			DNSResolvers:          "",
			AllowedDomains:        "",
			CloudflareAPIToken:    "",
		},

		API: APIConfig{
			Hostname:     "",
			TLS:          true,
			RedirectHTTP: true,
		},

		Metrics: MetricsConfig{
			ListenAddress: "127.0.0.1",
			Port:          9464,
			VMLabelBudget: 200,
		},

		Otel: OtelConfig{
			Enabled:              false,
			Endpoint:             "127.0.0.1:4317",
			ServiceName:          "hypeman",
			ServiceInstanceID:    getHostname(),
			Insecure:             true,
			MetricExportInterval: "60s",
		},

		Logging: LoggingConfig{
			Level:          "info",
			MaxSize:        "50MB",
			MaxFiles:       1,
			RotateInterval: "5m",
		},

		Build: BuildConfig{
			MaxConcurrentSourceBuilds: 2,
			BuilderImage:              "", // empty = build from embedded Dockerfile on first run
			Timeout:                   600,
			SecretsDir:                "",
			DockerSocket:              "/var/run/docker.sock",
		},

		Registry: RegistryConfig{
			URL:        "localhost:8080",
			Insecure:   false,
			CACertFile: "",
		},

		Limits: LimitsConfig{
			MaxVcpusPerInstance:   16,
			MaxMemoryPerInstance:  "32GB",
			MaxTotalVolumeStorage: "",
			MaxConcurrentBuilds:   1,
			MaxOverlaySize:        "100GB",
			MaxImageStorage:       0.2,
		},

		Oversubscription: OversubscriptionConfig{
			CPU:     4.0,
			Memory:  1.0,
			Disk:    1.0,
			Network: 2.0,
			DiskIO:  2.0,
		},

		Capacity: CapacityConfig{
			Disk:    "",
			Network: "",
			DiskIO:  "",
		},

		Hypervisor: HypervisorConfig{
			Default:               "cloud-hypervisor",
			FirecrackerBinaryPath: "",
			Memory: HypervisorMemoryConfig{
				Enabled:            false,
				KernelPageInitMode: "hardened",
				ReclaimEnabled:     true,
				VZBalloonRequired:  true,
			},
		},

		GPU: GPUConfig{
			ProfileCacheTTL: "30m",
		},
	}
}

// Load loads configuration with the following precedence (highest to lowest):
//
//  1. Environment variables — uses double-underscore (__) as the nesting
//     separator: PORT, DATA_DIR, JWT_SECRET for top-level keys and
//     CADDY__LISTEN_ADDRESS, NETWORK__BRIDGE_NAME, etc. for nested keys.
//  2. YAML config file (if found)
//  3. Default values
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
	// The "__" delimiter maps double-underscore in env var names to nested
	// koanf key separators: CADDY__LISTEN_ADDRESS → caddy.listen_address.
	// Single underscores are preserved: JWT_SECRET → jwt_secret (top-level).
	envProvider := env.ProviderWithValue("", "__", func(key string, value string) (string, interface{}) {
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
	if strings.TrimSpace(c.Metrics.ListenAddress) == "" {
		return fmt.Errorf("metrics.listen_address must not be empty")
	}
	if c.Metrics.Port < 1 || c.Metrics.Port > 65535 {
		return fmt.Errorf("metrics.port must be between 1 and 65535, got %d", c.Metrics.Port)
	}
	if c.Metrics.VMLabelBudget <= 0 {
		return fmt.Errorf("metrics.vm_label_budget must be positive, got %d", c.Metrics.VMLabelBudget)
	}
	if c.Otel.MetricExportInterval != "" {
		if _, err := time.ParseDuration(c.Otel.MetricExportInterval); err != nil {
			return fmt.Errorf("otel.metric_export_interval must be a valid duration, got %q: %w", c.Otel.MetricExportInterval, err)
		}
	}
	if c.Oversubscription.CPU <= 0 {
		return fmt.Errorf("oversubscription.cpu must be positive, got %v", c.Oversubscription.CPU)
	}
	if c.Oversubscription.Memory <= 0 {
		return fmt.Errorf("oversubscription.memory must be positive, got %v", c.Oversubscription.Memory)
	}
	if c.Oversubscription.Disk <= 0 {
		return fmt.Errorf("oversubscription.disk must be positive, got %v", c.Oversubscription.Disk)
	}
	if c.Oversubscription.Network <= 0 {
		return fmt.Errorf("oversubscription.network must be positive, got %v", c.Oversubscription.Network)
	}
	if c.Oversubscription.DiskIO <= 0 {
		return fmt.Errorf("oversubscription.disk_io must be positive, got %v", c.Oversubscription.DiskIO)
	}
	if c.Network.UploadBurstMultiplier < 1 {
		return fmt.Errorf("network.upload_burst_multiplier must be >= 1, got %v", c.Network.UploadBurstMultiplier)
	}
	if c.Network.DownloadBurstMultiplier < 1 {
		return fmt.Errorf("network.download_burst_multiplier must be >= 1, got %v", c.Network.DownloadBurstMultiplier)
	}
	if c.Build.MaxConcurrentSourceBuilds <= 0 {
		return fmt.Errorf("build.max_concurrent_source_builds must be positive, got %d", c.Build.MaxConcurrentSourceBuilds)
	}
	if c.Build.Timeout <= 0 {
		return fmt.Errorf("build.timeout must be positive, got %d", c.Build.Timeout)
	}
	if c.Hypervisor.Memory.KernelPageInitMode != "performance" && c.Hypervisor.Memory.KernelPageInitMode != "hardened" {
		return fmt.Errorf("hypervisor.memory.kernel_page_init_mode must be one of {performance,hardened}, got %q", c.Hypervisor.Memory.KernelPageInitMode)
	}
	return nil
}
