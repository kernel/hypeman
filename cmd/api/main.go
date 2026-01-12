package main

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/c2h5oh/datasize"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/ghodss/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/kernel/hypeman"
	"github.com/kernel/hypeman/cmd/api/api"
	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/guest"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/kernel/hypeman/lib/instances"
	mw "github.com/kernel/hypeman/lib/middleware"
	"github.com/kernel/hypeman/lib/oapi"
	"github.com/kernel/hypeman/lib/otel"
	"github.com/kernel/hypeman/lib/vmm"
	"github.com/riandyrn/otelchi"
	"golang.org/x/sync/errgroup"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application terminated", "error", err)
		os.Exit(1)
	}
	slog.Info("main() exiting normally")
}

func run() error {
	// Load config early for OTel initialization
	cfg := config.Load()

	// Validate configuration before proceeding
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("invalid configuration: %w", err)
	}

	// Initialize OpenTelemetry (before wire initialization)
	otelCfg := otel.Config{
		Enabled:           cfg.OtelEnabled,
		Endpoint:          cfg.OtelEndpoint,
		ServiceName:       cfg.OtelServiceName,
		ServiceInstanceID: cfg.OtelServiceInstanceID,
		Insecure:          cfg.OtelInsecure,
		Version:           cfg.Version,
		Env:               cfg.Env,
	}

	otelProvider, otelShutdown, err := otel.Init(context.Background(), otelCfg)
	if err != nil {
		// Log warning but don't fail - graceful degradation
		slog.Warn("failed to initialize OpenTelemetry, continuing without telemetry", "error", err)
	}
	if otelShutdown != nil {
		defer func() {
			slog.Info("shutting down OpenTelemetry")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := otelShutdown(shutdownCtx); err != nil {
				slog.Warn("error shutting down OpenTelemetry", "error", err)
			}
			slog.Info("OpenTelemetry shutdown complete")
		}()
	}

	// Initialize guest and vmm metrics if OTel is enabled
	if otelProvider != nil && otelProvider.Meter != nil {
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
	if otelProvider != nil && otelProvider.LogHandler != nil {
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

	// Log OTel status
	if cfg.OtelEnabled {
		logger.Info("OpenTelemetry enabled", "endpoint", cfg.OtelEndpoint, "service", cfg.OtelServiceName)
	}

	// Validate JWT secret is configured
	if app.Config.JwtSecret == "" {
		logger.Warn("JWT_SECRET not configured - API authentication will fail")
	}

	// Verify KVM access (required for VM creation)
	if err := checkKVMAccess(); err != nil {
		return fmt.Errorf("KVM access check failed: %w\n\nEnsure:\n  1. KVM is enabled (check /dev/kvm exists)\n  2. User is in 'kvm' group: sudo usermod -aG kvm $USER\n  3. Log out and back in, or use: newgrp kvm", err)
	}
	logger.Info("KVM access verified")

	// Check if QEMU is available (optional - only warn if not present)
	if _, err := (&qemu.Starter{}).GetBinaryPath(nil, ""); err != nil {
		logger.Warn("QEMU not available - QEMU hypervisor will not work", "error", err)
	}

	// Validate log rotation config
	var logMaxSize datasize.ByteSize
	if err := logMaxSize.UnmarshalText([]byte(app.Config.LogMaxSize)); err != nil {
		return fmt.Errorf("invalid LOG_MAX_SIZE %q: %w", app.Config.LogMaxSize, err)
	}
	logRotateInterval, err := time.ParseDuration(app.Config.LogRotateInterval)
	if err != nil {
		return fmt.Errorf("invalid LOG_ROTATE_INTERVAL %q: %w", app.Config.LogRotateInterval, err)
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
	allInstances, err := app.InstanceManager.ListInstances(app.Ctx)
	if err != nil {
		// On error, skip TAP cleanup entirely to avoid crashing running VMs.
		// Pass nil to Initialize to skip cleanup.
		logger.Warn("failed to list instances for TAP cleanup, skipping cleanup", "error", err)
		preserveTAPs = nil
	} else {
		// Initialize to empty slice (not nil) so cleanup runs even with no running VMs
		preserveTAPs = []string{}
		for _, inst := range allInstances {
			if inst.State == instances.StateRunning || inst.State == instances.StateUnknown {
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

	// Initialize ingress manager (starts Caddy daemon and DNS server for dynamic upstreams)
	logger.Info("Initializing ingress manager...")
	if err := app.IngressManager.Initialize(app.Ctx); err != nil {
		logger.Error("failed to initialize ingress manager", "error", err)
		return fmt.Errorf("initialize ingress manager: %w", err)
	}
	logger.Info("Ingress manager initialized", "listen_addr", cfg.CaddyListenAddress, "admin", app.IngressManager.AdminURL())

	// Create router
	r := chi.NewRouter()

	// Prepare HTTP metrics middleware (applied inside API group, not globally)
	// Global application breaks WebSocket (Hijacker) and SSE (Flusher)
	var httpMetricsMw func(http.Handler) http.Handler
	if otelProvider != nil && otelProvider.Meter != nil {
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
		mw.ResolveResource(app.ApiService.NewResolvers(), api.ResolverErrorResponder),
	).Get("/instances/{id}/cp", app.ApiService.CpHandler)

	// OCI Distribution registry endpoints for image push (outside OpenAPI spec)
	r.Route("/v2", func(r chi.Router) {
		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(middleware.Logger)
		r.Use(middleware.Recoverer)
		r.Use(mw.JwtAuth(app.Config.JwtSecret))
		r.Mount("/", app.Registry.Handler())
	})

	// Authenticated API endpoints
	r.Group(func(r chi.Router) {
		// Common middleware
		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(middleware.Recoverer)

		// OpenTelemetry tracing middleware FIRST (creates span context)
		if cfg.OtelEnabled {
			r.Use(otelchi.Middleware(cfg.OtelServiceName, otelchi.WithChiRoutes(r)))
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

		r.Use(middleware.Timeout(60 * time.Second))

		// OpenAPI request validation with authentication
		validatorOptions := &nethttpmiddleware.Options{
			Options: openapi3filter.Options{
				AuthenticationFunc: mw.OapiAuthenticationFunc(app.Config.JwtSecret),
			},
			ErrorHandler: mw.OapiErrorHandler,
		}
		r.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, validatorOptions))

		// Resource resolver middleware - resolves IDs/names/prefixes before handlers
		// Enriches context with resolved resource and logger with resolved ID
		r.Use(mw.ResolveResource(app.ApiService.NewResolvers(), api.ResolverErrorResponder))

		// Setup strict handler
		strictHandler := oapi.NewStrictHandler(app.ApiService, nil)

		// Mount API routes (authentication now handled by validation middleware)
		oapi.HandlerWithOptions(strictHandler, oapi.ChiServerOptions{
			BaseRouter:  r,
			Middlewares: []oapi.MiddlewareFunc{},
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

	// Error group for coordinated shutdown
	grp, gctx := errgroup.WithContext(ctx)

	// Start build manager background services (vsock handler for builder VMs)
	if err := app.BuildManager.Start(gctx); err != nil {
		logger.Error("failed to start build manager", "error", err)
		return err
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

	// Shutdown handler
	grp.Go(func() error {
		<-gctx.Done()
		logger.Info("shutdown signal received")

		// Use WithoutCancel to preserve context values while preventing cancellation
		shutdownCtx := context.WithoutCancel(gctx)
		shutdownCtx, cancel := context.WithTimeout(shutdownCtx, 30*time.Second)
		defer cancel()

		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error("failed to shutdown http server", "error", err)
			return err
		}
		logger.Info("http server shutdown complete")

		// Shutdown ingress manager (stops Caddy if CADDY_STOP_ON_SHUTDOWN=true)
		if err := app.IngressManager.Shutdown(shutdownCtx); err != nil {
			logger.Error("failed to shutdown ingress manager", "error", err)
			// Don't return error - continue with shutdown
		} else {
			logger.Info("ingress manager shutdown complete")
		}

		return nil
	})

	// Log rotation scheduler
	grp.Go(func() error {
		ticker := time.NewTicker(logRotateInterval)
		defer ticker.Stop()

		logger.Info("log rotation scheduler started", "interval", app.Config.LogRotateInterval, "max_size", logMaxSize, "max_files", app.Config.LogMaxFiles)
		for {
			select {
			case <-gctx.Done():
				return nil
			case <-ticker.C:
				if err := app.InstanceManager.RotateLogs(gctx, int64(logMaxSize), app.Config.LogMaxFiles); err != nil {
					logger.Error("log rotation failed", "error", err)
				} else {
					logger.Info("log rotation completed", "max_size", logMaxSize, "max_files", app.Config.LogMaxFiles)
				}
			}
		}
	})

	err = grp.Wait()
	slog.Info("all goroutines finished")
	return err
}

// checkKVMAccess verifies KVM is available and the user has permission to use it
func checkKVMAccess() error {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("/dev/kvm not found - KVM not enabled or not supported")
		}
		if os.IsPermission(err) {
			return fmt.Errorf("permission denied accessing /dev/kvm - user not in 'kvm' group")
		}
		return fmt.Errorf("cannot access /dev/kvm: %w", err)
	}
	f.Close()
	return nil
}
