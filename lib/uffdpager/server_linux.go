//go:build linux

package uffdpager

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	hypotel "github.com/kernel/hypeman/lib/otel"
)

type server struct {
	dataDir       string
	versionKey    string
	cache         *PageCache
	controlSocket string
	sessionRoot   string
	httpServer    *http.Server

	mu       sync.Mutex
	sessions map[string]*session
	draining bool

	faults           atomic.Int64
	backingBytesRead atomic.Int64
	copies           atomic.Int64
	copyErrors       atomic.Int64

	activeFaults        atomic.Int64
	maxConcurrentFaults atomic.Int64
	faultNanos          atomic.Int64
	faultMaxNanos       atomic.Int64
	readPageNanos       atomic.Int64
	readPageMaxNanos    atomic.Int64
	backingReadNanos    atomic.Int64
	backingReadMaxNanos atomic.Int64
	copyNanos           atomic.Int64
	copyMaxNanos        atomic.Int64
}

type session struct {
	id                string
	instanceID        string
	backingMemoryPath string
	cacheKey          string
	socketPath        string
	listener          *net.UnixListener
	backingFile       *os.File
	server            *server
	done              chan struct{}
	closeOnce         sync.Once
	uffdFD            int
	conn              *net.UnixConn
}

func Main(args []string) error {
	fs := flag.NewFlagSet("hypeman-uffd-pager", flag.ContinueOnError)
	dataDir := fs.String("data-dir", "", "hypeman data directory")
	versionKey := fs.String("version-key", "", "pager version key")
	cacheMaxBytes := fs.Int64("cache-max-bytes", defaultCacheMaxBytes, "maximum shared page cache bytes")
	metricsAddr := fs.String("metrics-addr", "", "TCP address for Prometheus /metrics (empty disables the metrics server)")
	otelEndpoint := fs.String("otel-endpoint", "", "OTLP endpoint for push export (empty disables push; pull remains available when --metrics-addr is set)")
	otelInsecure := fs.Bool("otel-insecure", true, "use insecure transport for OTLP push")
	metricExportInterval := fs.String("otel-metric-export-interval", "", "OTLP metric export interval (Go duration, e.g. 30s)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if strings.TrimSpace(*dataDir) == "" {
		return fmt.Errorf("--data-dir is required")
	}
	if strings.TrimSpace(*versionKey) == "" {
		return fmt.Errorf("--version-key is required")
	}

	s := newServer(*dataDir, *versionKey, *cacheMaxBytes)

	metricsShutdown, err := s.startMetrics(*metricsAddr, *otelEndpoint, *otelInsecure, *metricExportInterval)
	if err != nil {
		return fmt.Errorf("start metrics: %w", err)
	}
	defer metricsShutdown()

	return s.run()
}

func (s *server) startMetrics(metricsAddr, otelEndpoint string, otelInsecure bool, metricExportInterval string) (func(), error) {
	if strings.TrimSpace(metricsAddr) == "" && strings.TrimSpace(otelEndpoint) == "" {
		return func() {}, nil
	}

	ctx := context.Background()
	provider, otelShutdown, err := hypotel.Init(ctx, hypotel.Config{
		Enabled:              strings.TrimSpace(otelEndpoint) != "",
		Endpoint:             otelEndpoint,
		Insecure:             otelInsecure,
		ServiceName:          "hypeman-uffd-pager",
		ServiceInstanceID:    s.versionKey,
		MetricExportInterval: metricExportInterval,
	})
	if err != nil {
		return nil, fmt.Errorf("initialize telemetry: %w", err)
	}

	if err := RegisterMetrics(provider.MeterFor("hypeman-uffd-pager"), s.versionKey, s.stats); err != nil {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = otelShutdown(shutdownCtx)
		return nil, fmt.Errorf("register uffd metrics: %w", err)
	}

	var metricsSrv *http.Server
	if strings.TrimSpace(metricsAddr) != "" {
		mux := http.NewServeMux()
		mux.Handle("/metrics", provider.MetricsHandler)
		metricsSrv = &http.Server{Addr: metricsAddr, Handler: mux}
		go func() {
			slog.Info("serving uffd pager metrics", "addr", metricsAddr, "path", "/metrics", "version_key", s.versionKey)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Error("uffd metrics server error", "error", err)
			}
		}()
	}

	return func() {
		if metricsSrv != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := metricsSrv.Shutdown(shutdownCtx); err != nil && !errors.Is(err, http.ErrServerClosed) {
				slog.Warn("uffd metrics server shutdown error", "error", err)
			}
		}
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := otelShutdown(shutdownCtx); err != nil {
			slog.Warn("uffd otel shutdown error", "error", err)
		}
	}, nil
}

func newServer(dataDir, versionKey string, cacheMaxBytes int64) *server {
	dir := pagerVersionDir(dataDir, versionKey)
	return &server{
		dataDir:       dataDir,
		versionKey:    versionKey,
		cache:         NewPageCache(cacheMaxBytes),
		controlSocket: filepath.Join(dir, controlSocketFile),
		sessionRoot:   filepath.Join(dir, sessionsDir),
		sessions:      make(map[string]*session),
	}
}

func (s *server) run() error {
	if err := os.MkdirAll(s.sessionRoot, 0755); err != nil {
		return fmt.Errorf("create uffd session directory: %w", err)
	}
	_ = os.Remove(s.controlSocket)
	listener, err := net.Listen("unix", s.controlSocket)
	if err != nil {
		return fmt.Errorf("listen on uffd control socket: %w", err)
	}
	defer listener.Close()

	_ = os.WriteFile(filepath.Join(filepath.Dir(s.controlSocket), pagerPIDFile), []byte(fmt.Sprintf("%d\n", os.Getpid())), 0644)

	router := chi.NewRouter()
	router.Get("/health", s.handleHealth)
	router.Get("/stats", s.handleStats)
	router.Post("/sessions", s.handleCreateSession)
	router.Post("/sessions/{id}/close", s.handleCloseSession)
	router.Post("/drain", s.handleDrain)

	s.httpServer = &http.Server{Handler: router}
	err = s.httpServer.Serve(listener)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
