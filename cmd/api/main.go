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
	"syscall"
	"time"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/ghodss/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	nethttpmiddleware "github.com/oapi-codegen/nethttp-middleware"
	"github.com/onkernel/hypeman"
	"github.com/onkernel/hypeman/cmd/api/api"
	"github.com/onkernel/hypeman/lib/instances"
	mw "github.com/onkernel/hypeman/lib/middleware"
	"github.com/onkernel/hypeman/lib/oapi"
	"golang.org/x/sync/errgroup"
)

func main() {
	if err := run(); err != nil {
		slog.Error("application terminated", "error", err)
		os.Exit(1)
	}
}

func run() error {
	// Initialize app with wire
	app, cleanup, err := initializeApp()
	if err != nil {
		return fmt.Errorf("initialize application: %w", err)
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(app.Ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	logger := app.Logger

	// Validate JWT secret is configured
	if app.Config.JwtSecret == "" {
		logger.Warn("JWT_SECRET not configured - API authentication will fail")
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
	// Get running instance IDs for TAP cleanup
	runningIDs := getRunningInstanceIDs(app)
	logger.Info("Initializing network manager...")
	if err := app.NetworkManager.Initialize(app.Ctx, runningIDs); err != nil {
		logger.Error("failed to initialize network manager", "error", err)
		return fmt.Errorf("initialize network manager: %w", err)
	}
	logger.Info("Network manager initialized")

	// Create router
	r := chi.NewRouter()

	// Load OpenAPI spec for request validation
	spec, err := oapi.GetSwagger()
	if err != nil {
		return fmt.Errorf("failed to load OpenAPI spec: %w", err)
	}

	// Clear servers to avoid host validation issues
	// See: https://github.com/oapi-codegen/nethttp-middleware#usage
	spec.Servers = nil

	// Custom exec endpoint (outside OpenAPI spec, uses WebSocket)
	r.With(
		middleware.RequestID,
		middleware.RealIP,
		middleware.Logger,
		middleware.Recoverer,
		mw.JwtAuth(app.Config.JwtSecret),
	).Get("/instances/{id}/exec", app.ApiService.ExecHandler)

	// Authenticated API endpoints
	r.Group(func(r chi.Router) {
		// Common middleware
		r.Use(middleware.RequestID)
		r.Use(middleware.RealIP)
		r.Use(middleware.Logger)
		r.Use(middleware.Recoverer)
		r.Use(middleware.Timeout(60 * time.Second))

		// OpenAPI request validation with authentication
		validatorOptions := &nethttpmiddleware.Options{
			Options: openapi3filter.Options{
				AuthenticationFunc: mw.OapiAuthenticationFunc(app.Config.JwtSecret),
			},
			ErrorHandler: mw.OapiErrorHandler,
		}
		r.Use(nethttpmiddleware.OapiRequestValidatorWithOptions(spec, validatorOptions))

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
		return nil
	})

	return grp.Wait()
}

// getRunningInstanceIDs returns IDs of instances currently in Running state
func getRunningInstanceIDs(app *application) []string {
	allInstances, err := app.InstanceManager.ListInstances(app.Ctx)
	if err != nil {
		return nil
	}
	var running []string
	for _, inst := range allInstances {
		if inst.State == instances.StateRunning {
			running = append(running, inst.Id)
		}
	}
	return running
}

