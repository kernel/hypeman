package providers

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/onkernel/hypeman/cmd/api/config"
	"github.com/onkernel/hypeman/lib/builds"
	"github.com/onkernel/hypeman/lib/devices"
	"github.com/onkernel/hypeman/lib/hypervisor"
	"github.com/onkernel/hypeman/lib/images"
	"github.com/onkernel/hypeman/lib/ingress"
	"github.com/onkernel/hypeman/lib/instances"
	"github.com/onkernel/hypeman/lib/logger"
	"github.com/onkernel/hypeman/lib/network"
	hypemanotel "github.com/onkernel/hypeman/lib/otel"
	"github.com/onkernel/hypeman/lib/paths"
	"github.com/onkernel/hypeman/lib/registry"
	"github.com/onkernel/hypeman/lib/resources"
	"github.com/onkernel/hypeman/lib/system"
	"github.com/onkernel/hypeman/lib/volumes"
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
func ProvideConfig() *config.Config {
	cfg := config.Load()
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
	return images.NewManager(p, cfg.MaxConcurrentBuilds, meter)
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
	// Parse max overlay size from config
	var maxOverlaySize datasize.ByteSize
	if err := maxOverlaySize.UnmarshalText([]byte(cfg.MaxOverlaySize)); err != nil {
		return nil, fmt.Errorf("failed to parse MAX_OVERLAY_SIZE '%s': %w (expected format like '100GB', '50G', '10GiB')", cfg.MaxOverlaySize, err)
	}

	// Parse max memory per instance (empty or "0" means unlimited)
	var maxMemoryPerInstance int64
	if cfg.MaxMemoryPerInstance != "" && cfg.MaxMemoryPerInstance != "0" {
		var memSize datasize.ByteSize
		if err := memSize.UnmarshalText([]byte(cfg.MaxMemoryPerInstance)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_MEMORY_PER_INSTANCE '%s': %w", cfg.MaxMemoryPerInstance, err)
		}
		maxMemoryPerInstance = int64(memSize)
	}

	// Parse max total memory (empty or "0" means unlimited)
	var maxTotalMemory int64
	if cfg.MaxTotalMemory != "" && cfg.MaxTotalMemory != "0" {
		var memSize datasize.ByteSize
		if err := memSize.UnmarshalText([]byte(cfg.MaxTotalMemory)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_TOTAL_MEMORY '%s': %w", cfg.MaxTotalMemory, err)
		}
		maxTotalMemory = int64(memSize)
	}

	limits := instances.ResourceLimits{
		MaxOverlaySize:       int64(maxOverlaySize),
		MaxVcpusPerInstance:  cfg.MaxVcpusPerInstance,
		MaxMemoryPerInstance: maxMemoryPerInstance,
		MaxTotalVcpus:        cfg.MaxTotalVcpus,
		MaxTotalMemory:       maxTotalMemory,
	}

	meter := otel.GetMeterProvider().Meter("hypeman")
	tracer := otel.GetTracerProvider().Tracer("hypeman")
	defaultHypervisor := hypervisor.Type(cfg.DefaultHypervisor)
	return instances.NewManager(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, defaultHypervisor, meter, tracer), nil
}

// ProvideVolumeManager provides the volume manager
func ProvideVolumeManager(p *paths.Paths, cfg *config.Config) (volumes.Manager, error) {
	// Parse max total volume storage (empty or "0" means unlimited)
	var maxTotalVolumeStorage int64
	if cfg.MaxTotalVolumeStorage != "" && cfg.MaxTotalVolumeStorage != "0" {
		var storageSize datasize.ByteSize
		if err := storageSize.UnmarshalText([]byte(cfg.MaxTotalVolumeStorage)); err != nil {
			return nil, fmt.Errorf("failed to parse MAX_TOTAL_VOLUME_STORAGE '%s': %w", cfg.MaxTotalVolumeStorage, err)
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

// ProvideIngressManager provides the ingress manager
func ProvideIngressManager(p *paths.Paths, cfg *config.Config, instanceManager instances.Manager) (ingress.Manager, error) {
	// Parse DNS provider - fail if invalid
	dnsProvider, err := ingress.ParseDNSProvider(cfg.AcmeDnsProvider)
	if err != nil {
		return nil, fmt.Errorf("invalid ACME_DNS_PROVIDER: %w", err)
	}

	// Validate DNS propagation timeout if set (must be a valid Go duration string)
	if cfg.DnsPropagationTimeout != "" {
		if _, err := time.ParseDuration(cfg.DnsPropagationTimeout); err != nil {
			return nil, fmt.Errorf("invalid DNS_PROPAGATION_TIMEOUT %q: %w (expected format like '2m', '120s', '1h')", cfg.DnsPropagationTimeout, err)
		}
	}

	// Use config value for internal DNS port, fall back to default (0 = random) if not set
	internalDNSPort := cfg.InternalDNSPort
	if internalDNSPort == 0 {
		internalDNSPort = ingress.DefaultDNSPort
	}

	ingressConfig := ingress.Config{
		ListenAddress:  cfg.CaddyListenAddress,
		AdminAddress:   cfg.CaddyAdminAddress,
		AdminPort:      cfg.CaddyAdminPort,
		DNSPort:        internalDNSPort,
		StopOnShutdown: cfg.CaddyStopOnShutdown,
		ACME: ingress.ACMEConfig{
			Email:                 cfg.AcmeEmail,
			DNSProvider:           dnsProvider,
			CA:                    cfg.AcmeCA,
			DNSPropagationTimeout: cfg.DnsPropagationTimeout,
			DNSResolvers:          cfg.DnsResolvers,
			AllowedDomains:        cfg.TlsAllowedDomains,
			CloudflareAPIToken:    cfg.CloudflareApiToken,
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
func ProvideBuildManager(p *paths.Paths, cfg *config.Config, instanceManager instances.Manager, volumeManager volumes.Manager, log *slog.Logger) (builds.Manager, error) {
	buildConfig := builds.Config{
		MaxConcurrentBuilds: cfg.MaxConcurrentSourceBuilds,
		BuilderImage:        cfg.BuilderImage,
		RegistryURL:         cfg.RegistryURL,
		DefaultTimeout:      cfg.BuildTimeout,
		RegistrySecret:      cfg.JwtSecret, // Use same secret for registry tokens
	}

	// Apply defaults if not set
	if buildConfig.MaxConcurrentBuilds == 0 {
		buildConfig.MaxConcurrentBuilds = 2
	}
	if buildConfig.BuilderImage == "" {
		buildConfig.BuilderImage = "hypeman/builder:latest"
	}
	if buildConfig.RegistryURL == "" {
		buildConfig.RegistryURL = "localhost:8080"
	}
	if buildConfig.DefaultTimeout == 0 {
		buildConfig.DefaultTimeout = 600
	}

	// Configure secret provider
	var secretProvider builds.SecretProvider
	if cfg.BuildSecretsDir != "" {
		secretProvider = builds.NewFileSecretProvider(cfg.BuildSecretsDir)
		log.Info("build secrets enabled", "dir", cfg.BuildSecretsDir)
	}

	meter := otel.GetMeterProvider().Meter("hypeman")
	return builds.NewManager(p, buildConfig, instanceManager, volumeManager, secretProvider, log, meter)
}
