package providers

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ingress"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	hypemanotel "github.com/kernel/hypeman/lib/otel"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/system"
	"github.com/kernel/hypeman/lib/vm_metrics"
	"github.com/kernel/hypeman/lib/volumes"
	"go.opentelemetry.io/otel"
)

// ProvideLogger provides a structured logger with subsystem-specific levels.
// Wraps with InstanceLogHandler to automatically write logs with "id" attribute
// to per-instance hypeman.log files.
func ProvideLogger(p *paths.Paths) *slog.Logger {
	cfg := logger.NewConfig()
	otelHandler := hypemanotel.GetGlobalLogHandler()
	baseLogger := logger.NewSubsystemLogger(logger.SubsystemAPI, cfg, otelHandler)

	// Wrap the handler with instance log handler for per-instance logging
	logPathFunc := func(id string) string {
		return p.InstanceHypemanLog(id)
	}
	instanceHandler := logger.NewInstanceLogHandler(baseLogger.Handler(), logPathFunc)

	return slog.New(instanceHandler)
}

// ProvideContext provides a context with logger attached
func ProvideContext(log *slog.Logger) context.Context {
	return logger.AddToContext(context.Background(), log)
}

// ProvideConfig provides the application configuration.
// Panics if configuration is invalid (prevents startup with bad config).
// Config path can be specified via CONFIG_PATH env var or defaults to platform-specific locations.
func ProvideConfig() *config.Config {
	configPath := os.Getenv("CONFIG_PATH")
	cfg, err := config.Load(configPath)
	if err != nil {
		panic(fmt.Sprintf("failed to load configuration: %v", err))
	}
	if err := cfg.Validate(); err != nil {
		panic(fmt.Sprintf("invalid configuration: %v", err))
	}
	return cfg
}

// ProvidePaths provides the paths abstraction
func ProvidePaths(cfg *config.Config) *paths.Paths {
	return paths.New(cfg.DataDir)
}

// ProvideImageManager provides the image manager
func ProvideImageManager(p *paths.Paths, cfg *config.Config) (images.Manager, error) {
	meter := otel.GetMeterProvider().Meter("hypeman")
	return images.NewManager(p, cfg.Limits.MaxConcurrentBuilds, meter)
}

// ProvideSystemManager provides the system manager
func ProvideSystemManager(p *paths.Paths) system.Manager {
	return system.NewManager(p)
}

// ProvideNetworkManager provides the network manager
func ProvideNetworkManager(p *paths.Paths, cfg *config.Config) network.Manager {
	meter := otel.GetMeterProvider().Meter("hypeman")
	return network.NewManager(p, cfg, meter)
}

// ProvideDeviceManager provides the device manager
func ProvideDeviceManager(p *paths.Paths) devices.Manager {
	return devices.NewManager(p)
}

// ProvideInstanceManager provides the instance manager
func ProvideInstanceManager(p *paths.Paths, cfg *config.Config, imageManager images.Manager, systemManager system.Manager, networkManager network.Manager, deviceManager devices.Manager, volumeManager volumes.Manager) (instances.Manager, error) {
	firecracker.SetCustomBinaryPath(cfg.Hypervisor.FirecrackerBinaryPath)

	// Parse max overlay size from config
	var maxOverlaySize datasize.ByteSize
	if err := maxOverlaySize.UnmarshalText([]byte(cfg.Limits.MaxOverlaySize)); err != nil {
		return nil, fmt.Errorf("failed to parse MAX_OVERLAY_SIZE '%s': %w (expected format like '100GB', '50G', '10GiB')", cfg.Limits.MaxOverlaySize, err)
	}

	// Parse max memory per instance (empty or "0" means unlimited)
	var maxMemoryPerInstance int64
	if cfg.Limits.MaxMemoryPerInstance != "" && cfg.Limits.MaxMemoryPerInstance != "0" {
		var memSize datasize.ByteSize
		if err := memSize.UnmarshalText([]byte(cfg.Limits.MaxMemoryPerInstance)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_MEMORY_PER_INSTANCE '%s': %w", cfg.Limits.MaxMemoryPerInstance, err)
		}
		maxMemoryPerInstance = int64(memSize)
	}

	// Note: Aggregate CPU/memory limits are now handled via oversubscription ratios
	// in the ResourceManager, wired up via SetResourceValidator after initialization.
	limits := instances.ResourceLimits{
		MaxOverlaySize:       int64(maxOverlaySize),
		MaxVcpusPerInstance:  cfg.Limits.MaxVcpusPerInstance,
		MaxMemoryPerInstance: maxMemoryPerInstance,
	}

	meter := otel.GetMeterProvider().Meter("hypeman")
	tracer := otel.GetTracerProvider().Tracer("hypeman")
	defaultHypervisor := hypervisor.Type(cfg.Hypervisor.Default)
	return instances.NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, defaultHypervisor, meter, tracer), nil
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(p *paths.Paths, cfg *config.Config) (volumes.Manager, error) {
	// Parse max total volume storage (empty or "0" means unlimited)
	var maxTotalVolumeStorage int64
	if cfg.Limits.MaxTotalVolumeStorage != "" && cfg.Limits.MaxTotalVolumeStorage != "0" {
		var storageSize datasize.ByteSize
		if err := storageSize.UnmarshalText([]byte(cfg.Limits.MaxTotalVolumeStorage)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_TOTAL_VOLUME_STORAGE '%s': %w", cfg.Limits.MaxTotalVolumeStorage, err)
		}
		maxTotalVolumeStorage = int64(storageSize)
	}

	meter := otel.GetMeterProvider().Meter("hypeman")
	return volumes.NewManager(p, maxTotalVolumeStorage, meter), nil
}

// ProvideRegistry provides the OCI registry for image push
func ProvideRegistry(p *paths.Paths, imageManager images.Manager) (*registry.Registry, error) {
	return registry.New(p, imageManager)
}

// ProvideResourceManager provides the resource manager for capacity tracking
func ProvideResourceManager(ctx context.Context, cfg *config.Config, p *paths.Paths, imageManager images.Manager, instanceManager instances.Manager, volumeManager volumes.Manager) (*resources.Manager, error) {
	mgr := resources.NewManager(cfg, p)

	// Managers implement the lister interfaces directly
	mgr.SetImageLister(imageManager)
	mgr.SetInstanceLister(instanceManager)
	mgr.SetVolumeLister(volumeManager)

	// Initialize resource discovery
	if err := mgr.Initialize(ctx); err != nil {
		return nil, fmt.Errorf("initialize resource manager: %w", err)
	}

	return mgr, nil
}

// ProvideVMMetricsManager provides the VM metrics manager for utilization tracking
func ProvideVMMetricsManager(instanceManager instances.Manager, cfg *config.Config, log *slog.Logger) (*vm_metrics.Manager, error) {
	mgr := vm_metrics.NewManager()
	mgr.SetVMLabelBudget(cfg.Metrics.VMLabelBudget)
	mgr.SetLogger(log)

	// Adapt instance manager to vm_metrics.InstanceSource interface
	adapter := vm_metrics.NewInstanceManagerAdapter(instanceManager)
	mgr.SetInstanceSource(adapter)

	// Initialize OTel metrics
	meter := otel.GetMeterProvider().Meter("hypeman")
	if err := mgr.InitializeOTel(meter); err != nil {
		return nil, fmt.Errorf("initialize vm metrics: %w", err)
	}

	return mgr, nil
}

// ProvideIngressManager provides the ingress manager
func ProvideIngressManager(p *paths.Paths, cfg *config.Config, instanceManager instances.Manager) (ingress.Manager, error) {
	// Parse DNS provider - fail if invalid
	dnsProvider, err := ingress.ParseDNSProvider(cfg.ACME.DNSProvider)
	if err != nil {
		return nil, fmt.Errorf("invalid ACME_DNS_PROVIDER: %w", err)
	}

	// Validate DNS propagation timeout if set (must be a valid Go duration string)
	if cfg.ACME.DNSPropagationTimeout != "" {
		if _, err := time.ParseDuration(cfg.ACME.DNSPropagationTimeout); err != nil {
			return nil, fmt.Errorf("invalid DNS_PROPAGATION_TIMEOUT %q: %w (expected format like '2m', '120s', '1h')", cfg.ACME.DNSPropagationTimeout, err)
		}
	}

	// Use config value for internal DNS port, fall back to default (0 = random) if not set
	internalDNSPort := cfg.Caddy.InternalDNSPort
	if internalDNSPort == 0 {
		internalDNSPort = ingress.DefaultDNSPort
	}

	// Parse API port from config
	apiPort := 8080 // default
	if cfg.Port != "" {
		if p, err := strconv.Atoi(cfg.Port); err == nil {
			apiPort = p
		}
	}

	ingressConfig := ingress.Config{
		ListenAddress:  cfg.Caddy.ListenAddress,
		AdminAddress:   cfg.Caddy.AdminAddress,
		AdminPort:      cfg.Caddy.AdminPort,
		DNSPort:        internalDNSPort,
		StopOnShutdown: cfg.Caddy.StopOnShutdown,
		ACME: ingress.ACMEConfig{
			Email:                 cfg.ACME.Email,
			DNSProvider:           dnsProvider,
			CA:                    cfg.ACME.CA,
			DNSPropagationTimeout: cfg.ACME.DNSPropagationTimeout,
			DNSResolvers:          cfg.ACME.DNSResolvers,
			AllowedDomains:        cfg.ACME.AllowedDomains,
			CloudflareAPIToken:    cfg.ACME.CloudflareAPIToken,
		},
		APIIngress: ingress.APIIngressConfig{
			Hostname:     cfg.API.Hostname,
			Port:         apiPort,
			TLS:          cfg.API.TLS,
			RedirectHTTP: cfg.API.RedirectHTTP,
		},
	}

	// Create OTEL logger for Caddy log forwarding (if OTEL is enabled)
	var otelLogger *slog.Logger
	if otelHandler := hypemanotel.GetGlobalLogHandler(); otelHandler != nil {
		logCfg := logger.NewConfig()
		otelLogger = logger.NewSubsystemLogger(logger.SubsystemCaddy, logCfg, otelHandler)
	}

	// IngressResolver from instances package implements ingress.InstanceResolver
	resolver := instances.NewIngressResolver(instanceManager)
	return ingress.NewManager(p, ingressConfig, resolver, otelLogger), nil
}

// ProvideBuildManager provides the build manager
func ProvideBuildManager(p *paths.Paths, cfg *config.Config, instanceManager instances.Manager, volumeManager volumes.Manager, imageManager images.Manager, log *slog.Logger) (builds.Manager, error) {
	// Read CA cert file if specified
	var registryCACert string
	if cfg.Registry.CACertFile != "" {
		certData, err := os.ReadFile(cfg.Registry.CACertFile)
		if err != nil {
			return nil, fmt.Errorf("read registry CA cert file: %w", err)
		}
		registryCACert = string(certData)
		log.Info("registry CA certificate loaded", "file", cfg.Registry.CACertFile)
	}

	// Rewrite localhost in RegistryURL to the subnet gateway IP so builder VMs
	// (which run in their own network namespace) can reach the host registry.
	// Inside a VM, "localhost" refers to the VM itself, not the host.
	registryURL := cfg.Registry.URL
	if registryURL == "" {
		registryURL = "localhost:8080"
	}
	if strings.HasPrefix(registryURL, "localhost:") || strings.HasPrefix(registryURL, "127.0.0.1:") {
		gateway := cfg.Network.SubnetGateway
		if gateway == "" {
			var err error
			gateway, err = network.DeriveGateway(cfg.Network.SubnetCIDR)
			if err != nil {
				return nil, fmt.Errorf("derive gateway for registry URL rewrite: %w", err)
			}
		}
		port := strings.SplitN(registryURL, ":", 2)[1]
		registryURL = gateway + ":" + port
		log.Info("rewrote registry URL for builder VMs", "original", cfg.Registry.URL, "rewritten", registryURL)
	}

	buildConfig := builds.Config{
		MaxConcurrentBuilds: cfg.Build.MaxConcurrentSourceBuilds,
		BuilderImage:        cfg.Build.BuilderImage,
		RegistryURL:         registryURL,
		RegistryInsecure:    cfg.Registry.Insecure,
		RegistryCACert:      registryCACert,
		DefaultTimeout:      cfg.Build.Timeout,
		RegistrySecret:      cfg.JwtSecret, // Use same secret for registry tokens
		DockerSocket:        cfg.Build.DockerSocket,
	}

	// Configure secret provider (use NoOpSecretProvider as fallback to avoid nil panics)
	var secretProvider builds.SecretProvider
	if cfg.Build.SecretsDir != "" {
		secretProvider = builds.NewFileSecretProvider(cfg.Build.SecretsDir)
		log.Info("build secrets enabled", "dir", cfg.Build.SecretsDir)
	} else {
		secretProvider = &builds.NoOpSecretProvider{}
	}

	meter := otel.GetMeterProvider().Meter("hypeman")
	return builds.NewManager(p, buildConfig, instanceManager, volumeManager, imageManager, secretProvider, log, meter)
}
