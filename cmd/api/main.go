package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/ghodss/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/kernel/hypeman"
	"github.com/kernel/hypeman/cmd/api/api"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/kernel/hypeman/lib/imageretention"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/instances"
	loglib "github.com/kernel/hypeman/lib/logger"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/ocicachegc"
	"github.com/kernel/hypeman/lib/otel"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/kernel/hypeman/lib/providers"
	"github.com/kernel/hypeman/lib/registry"
	"github.com/kernel/hypeman/lib/scopes"
	"github.com/kernel/hypeman/lib/uffdgraduate"
	"github.com/kernel/hypeman/lib/vmm"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/riandyrn/otelchi"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"
)

func timeoutNonStreamingRequests(timeout time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		timeoutHandler := middleware.Timeout(timeout)(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/logs") || strings.HasSuffix(r.URL.Path, "/events") {
				next.ServeHTTP(w, r)
				return
			}
			timeoutHandler.ServeHTTP(w, r)
		})
	}
}

func main() {
	if err := run(); err != nil {
		slog.Error("application terminated", "error", err)
		os.Exit(1)
	}
	slog.Info("main() exiting normally")
}

func metricsServerAddress(cfg *config.Config) string {
	return net.JoinHostPort(cfg.Metrics.ListenAddress, strconv.Itoa(cfg.Metrics.Port))
}

func newMetricsServer(addr string, handler http.Handler) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("/metrics", handler)
	return &http.Server{
		Addr:    addr,
		Handler: mux,
	}
}

type imageRetentionRunner interface {
	Run(ctx context.Context) error
}

func configureImageRetentionController(cfg *config.Config, imageManager images.Manager, instanceManager instances.Manager, logger *slog.Logger, meter metric.Meter) (imageRetentionRunner, error) {
	if cfg == nil || !cfg.Images.AutoDelete.Enabled {
		return nil, nil
	}

	unusedFor, err := time.ParseDuration(cfg.Images.AutoDelete.UnusedFor)
	if err != nil {
		return nil, fmt.Errorf("invalid images.auto_delete.unused_for %q: %w", cfg.Images.AutoDelete.UnusedFor, err)
	}

	controller, err := imageretention.NewController(paths.New(cfg.DataDir), imageManager, unusedFor, cfg.Images.AutoDelete.Allowed, logger, meter)
	if err != nil {
		return nil, err
	}
	if setter, ok := instanceManager.(instances.ImageUsageRecorderSetter); ok {
		setter.SetImageUsageRecorder(controller)
	}
	return controller, nil
}

func startImageRetentionController(grp *errgroup.Group, ctx context.Context, controller imageRetentionRunner) bool {
	if grp == nil || controller == nil {
		return false
	}
	grp.Go(func() error {
		return controller.Run(ctx)
	})
	return true
}

type ociCacheGCRunner interface {
	Run(ctx context.Context) error
}

// compositeOCICacheRoots fans the GC's extra-root query out to every source
// that tracks cache blobs outside index.json: the embedded registry's
// BuildKit cache tags and the push manager's in-flight push digests.
type compositeOCICacheRoots []ocicachegc.RootsProvider

func (c compositeOCICacheRoots) LiveCacheManifestDigests() []string {
	out := make([]string, 0)
	for _, roots := range c {
		if roots == nil {
			continue
		}
		out = append(out, roots.LiveCacheManifestDigests()...)
	}
	return out
}

func configureOCICacheGC(cfg *config.Config, roots ocicachegc.RootsProvider, logger *slog.Logger, meter metric.Meter, tracer trace.Tracer) (ociCacheGCRunner, error) {
	if cfg == nil || !cfg.Images.OCICacheGC.Enabled {
		return nil, nil
	}

	interval, err := time.ParseDuration(cfg.Images.OCICacheGC.Interval)
	if err != nil {
		return nil, fmt.Errorf("invalid images.oci_cache_gc.interval %q: %w", cfg.Images.OCICacheGC.Interval, err)
	}
	minBlobAge, err := time.ParseDuration(cfg.Images.OCICacheGC.MinBlobAge)
	if err != nil {
		return nil, fmt.Errorf("invalid images.oci_cache_gc.min_blob_age %q: %w", cfg.Images.OCICacheGC.MinBlobAge, err)
	}

	return ocicachegc.NewCollector(paths.New(cfg.DataDir), interval, minBlobAge, roots, logger, meter, tracer)
}

func startOCICacheGC(grp *errgroup.Group, ctx context.Context, runner ociCacheGCRunner) bool {
	if grp == nil || runner == nil {
		return false
	}
	grp.Go(func() error {
		return runner.Run(ctx)
	})
	return true
}

func configureUFFDGraduationController(cfg *config.Config, instanceManager instances.Manager, logger *slog.Logger) (*uffdgraduate.Controller, error) {
	g := cfg.Hypervisor.FirecrackerUFFDGraduation
	if !g.Enabled {
		return nil, nil
	}
	minSessionAge, err := time.ParseDuration(g.MinSessionAge)
	if err != nil {
		return nil, fmt.Errorf("invalid hypervisor.firecracker_uffd_graduation.min_session_age %q: %w", g.MinSessionAge, err)
	}
	scanInterval, err := time.ParseDuration(g.ScanInterval)
	if err != nil {
		return nil, fmt.Errorf("invalid hypervisor.firecracker_uffd_graduation.scan_interval %q: %w", g.ScanInterval, err)
	}
	completionTimeout, err := time.ParseDuration(g.CompletionTimeout)
	if err != nil {
		return nil, fmt.Errorf("invalid hypervisor.firecracker_uffd_graduation.completion_timeout %q: %w", g.CompletionTimeout, err)
	}
	return providers.ProvideUFFDGraduationController(instanceManager, uffdgraduate.Config{
		Enabled:           true,
		MinSessionAge:     minSessionAge,
		MaxConcurrent:     g.MaxConcurrent,
		ScanInterval:      scanInterval,
		CompletionTimeout: completionTimeout,
	}, logger), nil
}

func run() error {
	// Load config early for OTel initialization
	// Config path can be specified via CONFIG_PATH env var or defaults to platform-specific locations
	configPath := os.Getenv("CONFIG_PATH")
	cfg, err := config.Load(configPath)
	if err != nil {
		return fmt.Errorf("failed to load config: %w", err)
	}

	// Validate configuration before proceeding
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Configure GPU profile cache TTL
	devices.SetGPUProfileCacheTTL(cfg.GPU.ProfileCacheTTL)

	// Initialize OpenTelemetry (before wire initialization)
	otelCfg := otel.Config{
		Enabled:                  cfg.Otel.Enabled,
		Endpoint:                 cfg.Otel.Endpoint,
		ServiceName:              cfg.Otel.ServiceName,
		ServiceInstanceID:        cfg.Otel.ServiceInstanceID,
		Insecure:                 cfg.Otel.Insecure,
		MetricExportInterval:     cfg.Otel.MetricExportInterval,
		SuccessfulGetSampleRatio: cfg.Otel.SuccessfulGetSampleRatio,
		Version:                  cfg.Version,
		Env:                      cfg.Env,
	}

	otelProvider, otelShutdown, err := otel.Init(context.Background(), otelCfg)
	if err != nil {
		return fmt.Errorf("initialize telemetry: %w", err)
	}
	defer func() {
		slog.Info("shutting down OpenTelemetry")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			slog.Warn("error shutting down OpenTelemetry", "error", err)
		}
		slog.Info("OpenTelemetry shutdown complete")
	}()

	// Initialize guest and vmm metrics.
	if otelProvider.Meter != nil {
		guestMetrics, err := guest.NewMetrics(otelProvider.Meter)
		if err == nil {
			guest.SetMetrics(guestMetrics)
		}
		vmmMetrics, err := vmm.NewMetrics(otelProvider.Meter)
		if err == nil {
			vmm.SetMetrics(vmmMetrics)
		}
	}

	// Set global OTel log handler for logger package
	if otelProvider.LogHandler != nil {
		otel.SetGlobalLogHandler(otelProvider.LogHandler)
	}

	// Initialize app with wire
	app, cleanup, err := initializeApp()
	if err != nil {
		return fmt.Errorf("initialize application: %w", err)
	}
	defer func() {
		slog.Info("cleaning up application resources")
		cleanup()
		slog.Info("application cleanup complete")
	}()

	ctx, stop := signal.NotifyContext(app.Ctx, os.Interrupt, syscall.SIGTERM)
	defer func() {
		slog.Info("stopping signal handler")
		stop()
		slog.Info("signal handler stopped")
	}()

	logger := app.Logger

	resourceRefreshInterval, err := time.ParseDuration(app.Config.Metrics.ResourceRefreshInterval)
	if err != nil {
		return fmt.Errorf("invalid metrics resource refresh interval %q: %w", app.Config.Metrics.ResourceRefreshInterval, err)
	}
	allocationReconcileInterval, err := time.ParseDuration(app.Config.Metrics.AllocationReconcileInterval)
	if err != nil {
		return fmt.Errorf("invalid metrics allocation reconcile interval %q: %w", app.Config.Metrics.AllocationReconcileInterval, err)
	}
	if err := app.ResourceManager.StartMonitoring(ctx, otelProvider.Meter, resourceRefreshInterval); err != nil {
		return fmt.Errorf("start resource monitoring: %w", err)
	}
	if reconciler, ok := app.InstanceManager.(interface {
		StartAdmissionAllocationReconciler(context.Context, time.Duration)
	}); ok {
		reconciler.StartAdmissionAllocationReconciler(ctx, allocationReconcileInterval)
	}
	if reconciler, ok := app.InstanceManager.(interface {
		StartTAPGCReconciler(context.Context)
	}); ok {
		reconciler.StartTAPGCReconciler(ctx)
	}

	// Log OTel status
	if cfg.Otel.Enabled {
		logger.Info("OpenTelemetry push enabled", "endpoint", cfg.Otel.Endpoint, "service", cfg.Otel.ServiceName, "metric_export_interval", cfg.Otel.MetricExportInterval, "successful_get_sample_ratio", cfg.Otel.SuccessfulGetSampleRatio)
	} else {
		logger.Info("OpenTelemetry push disabled; Prometheus pull metrics remain available")
	}

	// Validate JWT secret is configured
	if app.Config.JwtSecret == "" {
		logger.Warn("JWT_SECRET not configured - API authentication will fail")
	}

	// Verify hypervisor access (KVM on Linux, Virtualization.framework on macOS)
	if err := checkHypervisorAccess(); err != nil {
		return fmt.Errorf("hypervisor access check failed: %w", err)
	}
	logger.Info("Hypervisor access verified", "type", hypervisorAccessCheckName())

	// Check if QEMU is available (optional - only warn if not present)
	if _, err := (&qemu.Starter{}).GetBinaryPath(nil, ""); err != nil {
		logger.Warn("QEMU not available - QEMU hypervisor will not work", "error", err)
	}

	// Validate log rotation config
	var logMaxSize datasize.ByteSize
	if err := logMaxSize.UnmarshalText([]byte(app.Config.Logging.MaxSize)); err != nil {
		return fmt.Errorf("invalid LOG_MAX_SIZE %q: %w", app.Config.Logging.MaxSize, err)
	}
	logRotateInterval, err := time.ParseDuration(app.Config.Logging.RotateInterval)
	if err != nil {
		return fmt.Errorf("invalid LOG_ROTATE_INTERVAL %q: %w", app.Config.Logging.RotateInterval, err)
	}

	// Ensure system files (kernel, initrd) exist before starting server
	logger.Info("Ensuring system files...")
	if err := app.SystemManager.EnsureSystemFiles(app.Ctx); err != nil {
		logger.Error("failed to ensure system files", "error", err)
		os.Exit(1)
	}
	kernelVer := app.SystemManager.GetDefaultKernelVersion()
	logger.Info("System files ready",
		"kernel", kernelVer)

	// Initialize network manager (creates default network if needed)
	// Get instance IDs that might have a running VMM for TAP cleanup safety.
	// Include Unknown state: we couldn't confirm their state, but they might still
	// have a running VMM. Better to leave a stale TAP than crash a running VM.
	var preserveTAPs []string
	allInstances, err := app.InstanceManager.ListInstances(app.Ctx, nil)
	if err != nil {
		// On error, skip TAP cleanup entirely to avoid crashing running VMs.
		// Pass nil to Initialize to skip cleanup.
		logger.Warn("failed to list instances for TAP cleanup, skipping cleanup", "error", err)
		preserveTAPs = nil
	} else {
		// Initialize to empty slice (not nil) so cleanup runs even with no running VMs
		preserveTAPs = []string{}
		for _, inst := range allInstances {
			if inst.State == instances.StateRunning || inst.State == instances.StateInitializing || inst.State == instances.StateUnknown {
				preserveTAPs = append(preserveTAPs, inst.Id)
			}
		}
	}
	logger.Info("Initializing network manager...")
	if err := app.NetworkManager.Initialize(app.Ctx, preserveTAPs); err != nil {
		logger.Error("failed to initialize network manager", "error", err)
		return fmt.Errorf("initialize network manager: %w", err)
	}

	// Set up HTB qdisc on bridge for network fair sharing
	networkCapacity := app.ResourceManager.NetworkCapacity()
	if err := app.NetworkManager.SetupHTB(app.Ctx, networkCapacity); err != nil {
		logger.Warn("failed to setup HTB on bridge (network rate limiting disabled)", "error", err)
	}

	// Reconcile device state (clears orphaned attachments from crashed VMs)
	// Set up liveness checker so device reconciliation can accurately detect orphaned attachments
	logger.Info("Reconciling device state...")
	livenessChecker := instances.NewLivenessChecker(app.InstanceManager)
	if livenessChecker != nil {
		app.DeviceManager.SetLivenessChecker(livenessChecker)
	}
	if err := app.DeviceManager.ReconcileDevices(app.Ctx); err != nil {
		logger.Error("failed to reconcile device state", "error", err)
		return fmt.Errorf("reconcile device state: %w", err)
	}

	// Reconcile mdev devices (clears orphaned vGPUs from previous runs)
	logger.Info("Reconciling mdev devices...")
	if err := devices.ReconcileMdevs(app.Ctx, nil); err != nil {
		// Log but don't fail - mdev cleanup is best-effort
		logger.Warn("failed to reconcile mdev devices", "error", err)
	}

	// Wire up resource validator for aggregate limit checking
	// This enables the instance manager to validate CPU, memory, network, and GPU
	// availability before creating or starting instances.
	app.InstanceManager.SetResourceValidator(app.ResourceManager)
	logger.Info("Resource validator configured")

	// Initialize ingress manager (starts Caddy daemon and DNS server for dynamic upstreams)
	logger.Info("Initializing ingress manager...")
	if err := app.IngressManager.Initialize(app.Ctx); err != nil {
		logger.Error("failed to initialize ingress manager", "error", err)
		return fmt.Errorf("initialize ingress manager: %w", err)
	}
	logger.Info("Ingress manager initialized", "listen_addr", cfg.Caddy.ListenAddress, "admin", app.IngressManager.AdminURL())

	// Create router
	r := chi.NewRouter()

	// Prepare HTTP metrics middleware (applied inside API group, not globally)
	// Global application breaks WebSocket (Hijacker) and SSE (Flusher)
	var httpMetricsMw func(http.Handler) http.Handler
	if otelProvider.Meter != nil {
		httpMetrics, err := mw.NewHTTPMetrics(otelProvider.Meter)
		if err == nil {
			httpMetricsMw = httpMetrics.Middleware
		}
	}

	// Create access logger with OTel handler for HTTP request logging with trace correlation
	var accessLogHandler slog.Handler
	if otelProvider != nil {
		accessLogHandler = otelProvider.LogHandler
	}
	accessLogger := mw.NewAccessLogger(accessLogHandler)

	// Load OpenAPI spec for request validation
	spec, err := oapi.GetSwagger()
	if err != nil {
		return fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}

	// Clear servers to avoid host validation issues
	// See: https://github.com/oapi-codegen/nethttp-middleware#usage
	spec.Servers = nil

	// Custom exec endpoint (outside OpenAPI spec, uses WebSocket)
	// Note: No otelchi here as WebSocket doesn't work well with tracing middleware
	r.With(
		middleware.RequestID,
		middleware.RealIP,
		middleware.Recoverer,
		mw.InjectLogger(logger),
		mw.AccessLogger(accessLogger),
		mw.JwtAuth(app.Config.JwtSecret),
		scopes.RequireScope(scopes.InstanceWrite),
		mw.ResolveResource(app.ApiService.NewResolvers(), api.ResolverErrorResponder),
	).Get("/instances/{id}/exec", app.ApiService.ExecHandler)

	// Custom cp endpoint (outside OpenAPI spec, uses WebSocket)
	r.With(
		middleware.RequestID,
		middleware.RealIP,
		middleware.Recoverer,
		mw.InjectLogger(logger),
		mw.AccessLogger(accessLogger),
		mw.JwtAuth(app.Config.JwtSecret),
		scopes.RequireScope(scopes.InstanceWrite),
		mw.ResolveResource(app.ApiService.NewResolvers(), api.ResolverErrorResponder),
	).Get("/instances/{id}/cp", app.ApiService.CpHandler)

	// Create builder VM resolver for secure token authentication
	// This validates that token requests from builder VMs are for their authorized repos only
	// Create token handler for Docker Registry Token Authentication
	// All clients must provide explicit credentials (Basic or Bearer auth with JWT)
	tokenHandler := registry.NewTokenHandler(app.Config.JwtSecret)

	// OCI Distribution registry endpoints for image push (outside OpenAPI spec)
	r.Route("/v2", func(r chi.Router) {
		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(middleware.Recoverer)
		if cfg.Otel.Enabled {
			r.Use(otelchi.Middleware(cfg.Otel.ServiceName, otelchi.WithChiRoutes(r)))
		}
		r.Use(mw.InjectLogger(logger))
		r.Use(mw.AccessLogger(accessLogger))
		r.Use(mw.JwtAuth(app.Config.JwtSecret))

		// Token endpoint for Docker Registry Token Authentication
		// This is called by clients (like BuildKit) after receiving a 401 with WWW-Authenticate
		r.Get("/token", tokenHandler.ServeHTTP)

		r.Mount("/", app.Registry.Handler())
	})

	// Authenticated API endpoints
	r.Group(func(r chi.Router) {
		// Common middleware
		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(middleware.Recoverer)

		// OpenTelemetry tracing middleware FIRST (creates span context)
		if cfg.Otel.Enabled {
			r.Use(otelchi.Middleware(cfg.Otel.ServiceName, otelchi.WithChiRoutes(r)))
		}

		// Inject logger into request context for handlers to use
		// Use app logger (not accessLogger) so the instance log handler is included
		r.Use(mw.InjectLogger(logger))

		// Access logger AFTER otelchi so trace context is available
		r.Use(mw.AccessLogger(accessLogger))
		if httpMetricsMw != nil {
			// Skip HTTP metrics for SSE streaming endpoints (logs)
			r.Use(func(next http.Handler) http.Handler {
				return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if strings.HasSuffix(r.URL.Path, "/logs") {
						next.ServeHTTP(w, r)
						return
					}
					httpMetricsMw(next).ServeHTTP(w, r)
				})
			})
		}

		// Streaming endpoints can remain active for longer than the request timeout.
		// In particular, cold builds routinely exceed 60 seconds while continuing
		// to emit events; cancelling the SSE request makes the CLI report failure
		// even though the build is still running.
		r.Use(timeoutNonStreamingRequests(60 * time.Second))

		// OpenAPI request validation with authentication
		validatorOptions := &nethttpmiddleware.Options{
			Options: openapi3filter.Options{
				AuthenticationFunc: mw.OapiAuthenticationFunc(app.Config.JwtSecret),
			},
			ErrorHandler: mw.OapiErrorHandler,
		}
		r.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, validatorOptions))

		// Scoped permissions — enforce per-route scope requirements
		r.Use(scopes.Middleware())

		// Resource resolver middleware - resolves IDs/names/prefixes before handlers
		// Enriches context with resolved resource and logger with resolved ID
		r.Use(mw.ResolveResource(app.ApiService.NewResolvers(), api.ResolverErrorResponder))

		// Setup strict handler
		strictHandler := oapi.NewStrictHandler(app.ApiService, nil)

		// Mount API routes (authentication now handled by validation middleware)
		oapi.HandlerWithOptions(strictHandler, oapi.ChiServerOptions{
			BaseRouter:  r,
			Middlewares: []oapi.MiddlewareFunc{api.NormalizeOptionalStandbyBody},
		})
	})

	// Unauthenticated endpoints (outside group)
	r.Get("/spec.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oai.openapi")
		w.Write(hypeman.OpenAPIYAML)
	})

	r.Get("/spec.json", func(w http.ResponseWriter, r *http.Request) {
		jsonData, err := yaml.YAMLToJSON(hypeman.OpenAPIYAML)
		if err != nil {
			http.Error(w, "Failed to convert YAML to JSON", http.StatusInternalServerError)
			logger.ErrorContext(r.Context(), "Failed to convert YAML to JSON", "error", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(jsonData)
	})

	r.Get("/swagger", api.SwaggerUIHandler)

	// Create HTTP server
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", app.Config.Port),
		Handler: r,
	}

	metricsAddr := metricsServerAddress(cfg)
	if otelProvider.MetricsHandler == nil {
		return fmt.Errorf("metrics handler is not initialized")
	}
	metricsSrv := newMetricsServer(metricsAddr, otelProvider.MetricsHandler)

	// Error group for coordinated shutdown
	grp, gctx := errgroup.WithContext(ctx)

	retentionController, err := configureImageRetentionController(
		app.Config,
		app.ImageManager,
		app.InstanceManager,
		logger,
		otelProvider.MeterFor(loglib.SubsystemImages),
	)
	if err != nil {
		return err
	}
	if startImageRetentionController(grp, gctx, retentionController) {
		logger.Info("image auto-delete enabled", "unused_for", app.Config.Images.AutoDelete.UnusedFor)
	}

	ociGC, err := configureOCICacheGC(
		app.Config,
		compositeOCICacheRoots{app.Registry, app.PushManager},
		logger,
		otelProvider.MeterFor(loglib.SubsystemImages),
		otelProvider.TracerFor(loglib.SubsystemImages),
	)
	if err != nil {
		return err
	}
	if startOCICacheGC(grp, gctx, ociGC) {
		logger.Info("oci cache gc enabled",
			"interval", app.Config.Images.OCICacheGC.Interval,
			"min_blob_age", app.Config.Images.OCICacheGC.MinBlobAge,
		)
	}

	// Start builders manager (reconcile builder state, idle reaper)
	if err := app.BuilderManager.Start(gctx); err != nil {
		logger.Error("failed to start builders manager", "error", err)
		return err
	}

	// Start build manager background services (vsock handler for builder VMs)
	if err := app.BuildManager.Start(gctx); err != nil {
		logger.Error("failed to start build manager", "error", err)
		return err
	}

	grp.Go(func() error {
		if app.GuestMemoryController == nil {
			return nil
		}
		logger.Info("starting guest memory controller")
		return app.GuestMemoryController.Start(gctx)
	})
	if app.AutoStandbyController != nil {
		grp.Go(func() error {
			logger.Info("starting auto-standby controller")
			return app.AutoStandbyController.Run(gctx)
		})
	}

	uffdGraduationController, err := configureUFFDGraduationController(app.Config, app.InstanceManager, logger)
	if err != nil {
		return err
	}
	if uffdGraduationController != nil {
		grp.Go(func() error {
			logger.Info("starting uffd graduation controller")
			return uffdGraduationController.Run(gctx)
		})
	}
	if app.HealthCheckController != nil {
		grp.Go(func() error {
			logger.Info("starting health check controller")
			return app.HealthCheckController.Run(gctx)
		})
	}
	if restartController, ok := app.InstanceManager.(interface {
		StartRestartPolicyController(context.Context) error
	}); ok {
		grp.Go(func() error {
			logger.Info("starting restart policy controller")
			return restartController.StartRestartPolicyController(gctx)
		})
	}

	// Run the server
	grp.Go(func() error {
		logger.Info("starting hypeman API", "port", app.Config.Port)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("http server error", "error", err)
			return err
		}
		return nil
	})

	grp.Go(func() error {
		logger.Info("starting metrics endpoint", "addr", metricsAddr, "path", "/metrics")
		if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("metrics server error", "error", err)
			return err
		}
		return nil
	})

	// Shutdown handler
	grp.Go(func() error {
		<-gctx.Done()
		logger.Info("shutdown signal received")

		// Use WithoutCancel to preserve context values while preventing cancellation
		shutdownCtx := context.WithoutCancel(gctx)
		shutdownCtx, cancel := context.WithTimeout(shutdownCtx, 30*time.Second)
		defer cancel()

		var shutdownErrs []error

		if err := srv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("failed to shutdown http server", "error", err)
			shutdownErrs = append(shutdownErrs, fmt.Errorf("shutdown http server: %w", err))
		} else {
			logger.Info("http server shutdown complete")
		}

		if err := metricsSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("failed to shutdown metrics server", "error", err)
			shutdownErrs = append(shutdownErrs, fmt.Errorf("shutdown metrics server: %w", err))
		} else {
			logger.Info("metrics server shutdown complete")
		}

		// Shutdown ingress manager (stops Caddy if CADDY_STOP_ON_SHUTDOWN=true)
		if err := app.IngressManager.Shutdown(shutdownCtx); err != nil {
			logger.Error("failed to shutdown ingress manager", "error", err)
			// Don't return error - continue with shutdown
		} else {
			logger.Info("ingress manager shutdown complete")
		}

		return errors.Join(shutdownErrs...)
	})

	// Log rotation scheduler
	grp.Go(func() error {
		ticker := time.NewTicker(logRotateInterval)
		defer ticker.Stop()

		logger.Info("log rotation scheduler started", "interval", app.Config.Logging.RotateInterval, "max_size", logMaxSize, "max_files", app.Config.Logging.MaxFiles)
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				if err := app.InstanceManager.RotateLogs(gctx, int64(logMaxSize), app.Config.Logging.MaxFiles); err != nil {
					logger.Error("log rotation failed", "error", err)
				} else {
					logger.Debug("log rotation completed", "max_size", logMaxSize, "max_files", app.Config.Logging.MaxFiles)
				}
			}
		}
	})

	// Snapshot schedule scheduler
	if scheduleManager, ok := app.InstanceManager.(instances.SnapshotScheduleManager); ok {
		const snapshotSchedulePollInterval = time.Minute
		grp.Go(func() error {
			ticker := time.NewTicker(snapshotSchedulePollInterval)
			defer ticker.Stop()

			logger.Info("snapshot schedule scheduler started", "interval", snapshotSchedulePollInterval)
			for {
				select {
				case <-gctx.Done():
					return nil
				case <-ticker.C:
					if err := scheduleManager.RunSnapshotSchedules(gctx); err != nil {
						logger.Error("snapshot schedule run completed with errors", "error", err)
					}
				}
			}
		})
	} else {
		logger.Warn("snapshot schedule manager unavailable; scheduled snapshots disabled")
	}

	err = grp.Wait()
	slog.Info("all goroutines finished")
	return err
}
