// Command hypeman-auth is the OAuth authorization server for Hypeman. It
// exchanges OIDC identity tokens for short-lived access tokens and publishes
// the keys Hypeman servers verify them with.
package main

import (
	"context"
	"crypto/ecdsa"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/kernel/hypeman/cmd/auth/config"
	"github.com/kernel/hypeman/lib/authserver"
	"github.com/kernel/hypeman/lib/oidcfederation"
	"github.com/kernel/hypeman/lib/otel"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("hypeman-auth stopped", "error", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load(os.Getenv("CONFIG_PATH"))
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	telemetryConfig := otel.Config{ServiceName: "hypeman-auth"}
	if cfg.OtelEndpoint != "" {
		// Only the scheme, host and port reach the gRPC client, so anything
		// else in the URL would be dropped without a word.
		endpoint, err := url.Parse(cfg.OtelEndpoint)
		if err != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
			endpoint.User != nil || strings.Trim(endpoint.EscapedPath(), "/") != "" || strings.ContainsAny(cfg.OtelEndpoint, "?#") {
			return errors.New("otel_endpoint must be http or https with a host and an optional port, and nothing else")
		}
		telemetryConfig.Enabled, telemetryConfig.Endpoint, telemetryConfig.Insecure = true, endpoint.Host, endpoint.Scheme == "http"
	}
	telemetry, shutdownTelemetry, err := otel.Init(ctx, telemetryConfig)
	if err != nil {
		return fmt.Errorf("initialize telemetry: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTelemetry(shutdownCtx)
	}()

	encoded, err := cfg.SigningKey.Read()
	if err != nil {
		return fmt.Errorf("signing_key: %w", err)
	}
	key, err := authserver.ParseSigningKey(encoded)
	if err != nil {
		return fmt.Errorf("signing_key: %w", err)
	}
	var published []*ecdsa.PublicKey
	for i, entry := range cfg.PublishedKeys {
		encoded, err := entry.Read()
		if err != nil {
			return fmt.Errorf("published_keys[%d]: %w", i, err)
		}
		public, err := authserver.ParsePublicKey(encoded)
		if err != nil {
			return fmt.Errorf("published_keys[%d]: %w", i, err)
		}
		published = append(published, public)
	}
	rules, err := oidcfederation.Compile(cfg.OIDC)
	if err != nil {
		return err
	}
	options := authserver.Options{Issuer: cfg.Issuer, Key: key, PublishedKeys: published, Rules: rules, Meter: telemetry.Meter, Log: log}
	if cfg.ReplayStore.PostgresURL != "" {
		openCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		store, err := authserver.OpenPostgresStore(openCtx, cfg.ReplayStore.PostgresURL)
		cancel()
		if err != nil {
			return err
		}
		defer store.Close()
		go store.Cleanup(ctx, log)
		options.Spent = store
	}
	server, err := authserver.New(options)
	if err != nil {
		return err
	}

	// No route streams, so the whole request and response are bounded here.
	// The token endpoint gives itself 20 seconds from the end of the request
	// header, which the write deadline and the drain below leave room for.
	api := &http.Server{
		Addr:              cfg.ListenAddress,
		Handler:           server.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	servers := []*http.Server{api}
	if cfg.MetricsListenAddress != "" {
		mux := http.NewServeMux()
		mux.Handle("GET /metrics", telemetry.MetricsHandler)
		servers = append(servers, &http.Server{Addr: cfg.MetricsListenAddress, Handler: mux, ReadHeaderTimeout: 5 * time.Second})
	}
	failed := make(chan error, len(servers))
	for _, s := range servers {
		go func() {
			if err := s.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
				failed <- fmt.Errorf("listen on %s: %w", s.Addr, err)
			}
		}()
	}
	log.Info("hypeman-auth started", "issuer", cfg.Issuer, "listen_address", cfg.ListenAddress, "key_id", key.ID, "published_keys", len(published), "federation_rules", len(cfg.OIDC.FederationRules), "replay_store", options.Spent != nil)

	select {
	case err = <-failed:
	case <-ctx.Done():
	}
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, s := range servers {
		if shutdownErr := s.Shutdown(shutdownCtx); shutdownErr != nil {
			log.Warn("requests were still running when the server stopped", "listen_address", s.Addr, "error", shutdownErr)
		}
	}
	return err
}
