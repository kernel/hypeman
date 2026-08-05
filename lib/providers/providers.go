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
	"github.com/kernel/hypeman/lib/builders"
	"github.com/kernel/hypeman/lib/builds"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guestmemory"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/cloudhypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/firecracker"
	"github.com/kernel/hypeman/lib/imagepush"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/ingress"
	"github.com/kernel/hypeman/lib/instances"
	"github.com/kernel/hypeman/lib/logger"
	"github.com/kernel/hypeman/lib/network"
	hypemanotel "github.com/kernel/hypeman/lib/otel"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/kernel/hypeman/lib/resources"
	"github.com/kernel/hypeman/lib/snapshot"
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
	if err := cloudhypervisor.SetDefaultVersion(cfg.Hypervisor.CloudHypervisorDefaultVersion); err != nil {
		return nil, fmt.Errorf("invalid cloud-hypervisor default version: %w", err)
	}

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
	tracer := otel.GetTracerProvider().Tracer("hypeman/instances")
	defaultHypervisor := hypervisor.Type(cfg.Hypervisor.Default)
	snapshotDefaults := snapshotDefaultsFromConfig(cfg)
	memoryPolicy := guestmemory.Policy{
		Enabled:            cfg.Hypervisor.Memory.Enabled,
		KernelPageInitMode: guestmemory.KernelPageInitMode(cfg.Hypervisor.Memory.KernelPageInitMode),
		ReclaimEnabled:     cfg.Hypervisor.Memory.ReclaimEnabled,
		VZBalloonRequired:  cfg.Hypervisor.Memory.VZBalloonRequired,
	}
	var firecrackerUFFDCacheMaxBytes datasize.ByteSize
	if err := firecrackerUFFDCacheMaxBytes.UnmarshalText([]byte(cfg.Hypervisor.FirecrackerUFFDCacheMaxBytes)); err != nil {
		return nil, fmt.Errorf("failed to parse hypervisor.firecracker_uffd_cache_max_bytes %q: %w", cfg.Hypervisor.FirecrackerUFFDCacheMaxBytes, err)
	}
	managerConfig := instances.ManagerConfig{
		LifecycleEventBufferSize:         cfg.Instances.LifecycleEventBufferSize,
		FirecrackerSnapshotMemoryBackend: cfg.Hypervisor.FirecrackerSnapshotMemoryBackend,
		FirecrackerUFFDCacheMaxBytes:     int64(firecrackerUFFDCacheMaxBytes),
		MaxConcurrentRestoresByHypervisor: map[hypervisor.Type]int{
			hypervisor.TypeFirecracker: cfg.Hypervisor.FirecrackerMaxConcurrentRestores,
		},
	}
	return instances.NewManagerWithConfigE(p, imageManager, systemManager, networkManager, deviceManager, volumeManager, limits, defaultHypervisor, snapshotDefaults, managerConfig, meter, tracer, memoryPolicy)
}

func snapshotDefaultsFromConfig(cfg *config.Config) instances.SnapshotPolicy {
	if !cfg.Snapshot.CompressionDefault.Enabled {
		return instances.SnapshotPolicy{}
	}

	algorithm := snapshot.SnapshotCompressionAlgorithm(strings.ToLower(cfg.Snapshot.CompressionDefault.Algorithm))
	compression := &snapshot.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: algorithm,
	}
	if cfg.Snapshot.CompressionDefault.Level != nil {
		level := *cfg.Snapshot.CompressionDefault.Level
		compression.Level = &level
	}
	return instances.SnapshotPolicy{Compression: compression}
}

// ProvideGuestMemoryController provides the active ballooning controller.
func ProvideGuestMemoryController(instanceManager instances.Manager, cfg *config.Config, log *slog.Logger) (guestmemory.Controller, error) {
	pollInterval, err := parseRequiredDuration(cfg.Hypervisor.Memory.ActiveBallooning.PollInterval)
	if err != nil {
		return nil, fmt.Errorf("parse active ballooning poll interval: %w", err)
	}
	perVMCooldown, err := parseRequiredDuration(cfg.Hypervisor.Memory.ActiveBallooning.PerVmCooldown)
	if err != nil {
		return nil, fmt.Errorf("parse active ballooning per-vm cooldown: %w", err)
	}
	protectedFloorMinBytes, err := parseByteSize(cfg.Hypervisor.Memory.ActiveBallooning.ProtectedFloorMinBytes)
	if err != nil {
		return nil, fmt.Errorf("parse active ballooning protected floor: %w", err)
	}
	minAdjustmentBytes, err := parseByteSize(cfg.Hypervisor.Memory.ActiveBallooning.MinAdjustmentBytes)
	if err != nil {
		return nil, fmt.Errorf("parse active ballooning min adjustment: %w", err)
	}
	perVMMaxStepBytes, err := parseByteSize(cfg.Hypervisor.Memory.ActiveBallooning.PerVmMaxStepBytes)
	if err != nil {
		return nil, fmt.Errorf("parse active ballooning per-vm max step: %w", err)
	}

	policy := guestmemory.Policy{
		Enabled:            cfg.Hypervisor.Memory.Enabled,
		KernelPageInitMode: guestmemory.KernelPageInitMode(cfg.Hypervisor.Memory.KernelPageInitMode),
		ReclaimEnabled:     cfg.Hypervisor.Memory.ReclaimEnabled,
		VZBalloonRequired:  cfg.Hypervisor.Memory.VZBalloonRequired,
	}

	controllerCfg := guestmemory.ActiveBallooningConfig{
		Enabled:                               cfg.Hypervisor.Memory.ActiveBallooning.Enabled,
		PollInterval:                          pollInterval,
		PressureHighWatermarkAvailablePercent: cfg.Hypervisor.Memory.ActiveBallooning.PressureHighWatermarkAvailablePercent,
		PressureLowWatermarkAvailablePercent:  cfg.Hypervisor.Memory.ActiveBallooning.PressureLowWatermarkAvailablePercent,
		ProtectedFloorPercent:                 cfg.Hypervisor.Memory.ActiveBallooning.ProtectedFloorPercent,
		ProtectedFloorMinBytes:                protectedFloorMinBytes,
		MinAdjustmentBytes:                    minAdjustmentBytes,
		PerVMMaxStepBytes:                     perVMMaxStepBytes,
		PerVMCooldown:                         perVMCooldown,
	}

	return guestmemory.NewController(policy, controllerCfg, &guestMemoryInstanceSource{manager: instanceManager}, log.With("component", "guestmemory")), nil
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

func parseRequiredDuration(value string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, fmt.Errorf("must not be empty")
	}
	return time.ParseDuration(value)
}

// ProvideRegistry provides the OCI registry for image push
func ProvideRegistry(p *paths.Paths, imageManager images.Manager) (*registry.Registry, error) {
	return registry.New(p, imageManager)
}

// ProvidePushManager provides the manager for outbound image pushes to remote
// registries. Credentials default to the server's Docker keychain; API callers
// can instead lend per-request credentials, which the manager borrows for the
// duration of a single push without persisting them.
func ProvidePushManager(p *paths.Paths, cfg *config.Config, imageManager images.Manager) (imagepush.Manager, error) {
	return imagepush.NewManager(p, imageManager, nil, cfg.Limits.MaxConcurrentPushes)
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

type guestMemoryInstanceSource struct {
	manager instances.Manager
}

func (s *guestMemoryInstanceSource) ListBalloonVMs(ctx context.Context) ([]guestmemory.BalloonVM, error) {
	insts, err := s.manager.ListInstances(ctx, nil)
	if err != nil {
		return nil, err
	}

	vms := make([]guestmemory.BalloonVM, 0, len(insts))
	for _, inst := range insts {
		if inst.State != instances.StateRunning && inst.State != instances.StateInitializing {
			continue
		}
		vms = append(vms, guestmemory.BalloonVM{
			ID:                  inst.Id,
			Name:                inst.Name,
			HypervisorType:      inst.HypervisorType,
			SocketPath:          inst.SocketPath,
			AssignedMemoryBytes: inst.Size + inst.HotplugSize,
		})
	}
	return vms, nil
}

func parseByteSize(value string) (int64, error) {
	var size datasize.ByteSize
	if err := size.UnmarshalText([]byte(value)); err != nil {
		return 0, err
	}
	return int64(size), nil
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
	apiPort := 4973 // default
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

// ProvideBuilderManager provides the builders manager
func ProvideBuilderManager(p *paths.Paths, cfg *config.Config, instanceManager instances.Manager, volumeManager volumes.Manager, log *slog.Logger) (builders.Manager, error) {
	meter := otel.GetMeterProvider().Meter("hypeman")
	return builders.NewManager(p, builders.Config{
		MaxCount:          cfg.Builders.MaxCount,
		DefaultDiskSizeGb: cfg.Builders.DefaultDiskSizeGb,
		MaxDiskSizeGb:     cfg.Builders.MaxDiskSizeGb,
		IdleTTL:           cfg.Builders.IdleTTLDuration,
	}, volumeManager, instanceManager, log, meter)
}

// ProvideBuildManager provides the build manager
func ProvideBuildManager(p *paths.Paths, cfg *config.Config, instanceManager instances.Manager, volumeManager volumes.Manager, builderManager builders.Manager, imageManager images.Manager, networkManager network.Manager, log *slog.Logger) (builds.Manager, error) {
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

	registryURL, err := builderRegistryURL(cfg.Registry.URL, networkManager)
	if err != nil {
		return nil, err
	}
	if registryURL != cfg.Registry.URL {
		log.Info("resolved registry URL for builder VMs", "original", cfg.Registry.URL, "resolved", registryURL)
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
	buildManager, err := builds.NewManager(p, buildConfig, instanceManager, volumeManager, builderManager, imageManager, secretProvider, log, meter)
	if err != nil {
		return nil, err
	}

	// Builder delete and prune reject while builds are queued or running.
	builderManager.SetBuildActivityChecker(buildManager.BuilderHasBuilds)

	return buildManager, nil
}

// builderRegistryURL rewrites loopback registry addresses to the host gateway
// visible to guests. The network manager owns platform-specific gateway selection.
func builderRegistryURL(configuredURL string, networkManager network.Manager) (string, error) {
	registryURL := configuredURL
	if registryURL == "" {
		registryURL = "localhost:4973"
	}
	if !strings.HasPrefix(registryURL, "localhost:") && !strings.HasPrefix(registryURL, "127.0.0.1:") {
		return registryURL, nil
	}

	defaultNetwork, err := networkManager.EffectiveDefaultNetwork()
	if err != nil {
		return "", fmt.Errorf("get effective default network for registry URL rewrite: %w", err)
	}
	port := strings.SplitN(registryURL, ":", 2)[1]
	return defaultNetwork.Gateway + ":" + port, nil
}
