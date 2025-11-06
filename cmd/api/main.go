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

	"github.com/ghodss/yaml"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	openapi "github.com/onkernel/cloud-hypervisor-dataplane"
	"github.com/onkernel/cloud-hypervisor-dataplane/lib/oapi"
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

	// Create router
	r := chi.NewRouter()

	// Middleware
	r.Use(middleware.RequestID)
	r.Use(middleware.RealIP)
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)
	r.Use(middleware.Timeout(60 * time.Second))

	// Serve OpenAPI spec
	r.Get("/spec.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.oai.openapi")
		w.Write(openapi.OpenAPIYAML)
	})

	r.Get("/spec.json", func(w http.ResponseWriter, r *http.Request) {
		jsonData, err := yaml.YAMLToJSON(openapi.OpenAPIYAML)
		if err != nil {
			http.Error(w, "Failed to convert YAML to JSON", http.StatusInternalServerError)
			logger.ErrorContext(r.Context(), "Failed to convert YAML to JSON", "error", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(jsonData)
	})

	// Setup strict handler
	strictHandler := oapi.NewStrictHandler(app.ApiService, nil)

	// Mount API routes
	oapi.HandlerWithOptions(strictHandler, oapi.ChiServerOptions{
		BaseRouter: r,
	})

	// Create HTTP server
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%s", app.Config.Port),
		Handler: r,
	}

	// Error group for coordinated shutdown
	grp, gctx := errgroup.WithContext(ctx)

	// Run the server
	grp.Go(func() error {
		logger.Info("starting cloud hypervisor dataplane API", "port", app.Config.Port)
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

		shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
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

