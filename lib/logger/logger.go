// Package logger provides structured logging with subsystem-specific levels
// and OpenTelemetry trace context integration.
package logger

import (
	"context"
	"log/slog"
	"os"
	"strings"

	"go.opentelemetry.io/otel/trace"
)

type contextKey string

const loggerKey contextKey = "logger"

// Subsystem names for per-subsystem logging configuration.
const (
	SubsystemAPI       = "API"
	SubsystemCaddy     = "CADDY"
	SubsystemImages    = "IMAGES"
	SubsystemIngress   = "INGRESS"
	SubsystemInstances = "INSTANCES"
	SubsystemNetwork   = "NETWORK"
	SubsystemVolumes   = "VOLUMES"
	SubsystemVMM       = "VMM"
	SubsystemSystem    = "SYSTEM"
	SubsystemExec      = "EXEC"
)

// Config holds logging configuration.
type Config struct {
	// DefaultLevel is the default log level for all subsystems.
	DefaultLevel slog.Level
	// SubsystemLevels maps subsystem names to their specific log levels.
	// If a subsystem is not in this map, DefaultLevel is used.
	SubsystemLevels map[string]slog.Level
	// AddSource adds source file information to log entries.
	AddSource bool
}

// NewConfig creates a Config from environment variables.
// Reads LOG_LEVEL for default level and LOG_LEVEL_<SUBSYSTEM> for per-subsystem levels.
func NewConfig() Config {
	cfg := Config{
		DefaultLevel:    slog.LevelInfo,
		SubsystemLevels: make(map[string]slog.Level),
		AddSource:       false,
	}

	// Parse default level
	if levelStr := os.Getenv("LOG_LEVEL"); levelStr != "" {
		cfg.DefaultLevel = parseLevel(levelStr)
	}

	// Parse subsystem-specific levels
	subsystems := []string{
		SubsystemAPI, SubsystemCaddy, SubsystemImages, SubsystemIngress,
		SubsystemInstances, SubsystemNetwork, SubsystemVolumes, SubsystemVMM,
		SubsystemSystem, SubsystemExec,
	}
	for _, subsystem := range subsystems {
		envKey := "LOG_LEVEL_" + subsystem
		if levelStr := os.Getenv(envKey); levelStr != "" {
			cfg.SubsystemLevels[subsystem] = parseLevel(levelStr)
		}
	}

	return cfg
}

// parseLevel parses a log level string.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(s) {
	case "debug":
		return slog.LevelDebug
	case "info":
		return slog.LevelInfo
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// LevelFor returns the log level for the given subsystem.
func (c Config) LevelFor(subsystem string) slog.Level {
	if level, ok := c.SubsystemLevels[subsystem]; ok {
		return level
	}
	return c.DefaultLevel
}

// NewLogger creates a new slog.Logger with JSON output.
func NewLogger(cfg Config) *slog.Logger {
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     cfg.DefaultLevel,
		AddSource: cfg.AddSource,
	}))
}

// NewSubsystemLogger creates a logger for a specific subsystem with its configured level.
// If otelHandler is provided, logs will be sent both to stdout and to OTel.
func NewSubsystemLogger(subsystem string, cfg Config, otelHandler slog.Handler) *slog.Logger {
	level := cfg.LevelFor(subsystem)
	jsonHandler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: cfg.AddSource,
	})

	var baseHandler slog.Handler
	if otelHandler != nil {
		// Use multi-handler to write to both stdout and OTel
		baseHandler = &multiHandler{
			handlers: []slog.Handler{jsonHandler, otelHandler},
		}
	} else {
		baseHandler = jsonHandler
	}

	// Wrap with trace context handler for trace IDs in logs
	wrappedHandler := &traceContextHandler{
		Handler:   baseHandler,
		subsystem: subsystem,
		level:     level,
	}
	return slog.New(wrappedHandler)
}

// traceContextHandler wraps a slog.Handler to add trace context and subsystem.
type traceContextHandler struct {
	slog.Handler
	subsystem string
	level     slog.Level
}

// Enabled reports whether the handler handles records at the given level.
func (h *traceContextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return level >= h.level
}

// Handle adds trace_id and span_id from the context if available.
func (h *traceContextHandler) Handle(ctx context.Context, r slog.Record) error {
	// Add subsystem attribute
	r.AddAttrs(slog.String("subsystem", h.subsystem))

	// Add trace context if available
	spanCtx := trace.SpanContextFromContext(ctx)
	if spanCtx.IsValid() {
		r.AddAttrs(
			slog.String("trace_id", spanCtx.TraceID().String()),
			slog.String("span_id", spanCtx.SpanID().String()),
		)
	}

	return h.Handler.Handle(ctx, r)
}

// WithAttrs returns a new handler with the given attributes.
func (h *traceContextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &traceContextHandler{
		Handler:   h.Handler.WithAttrs(attrs),
		subsystem: h.subsystem,
		level:     h.level,
	}
}

// WithGroup returns a new handler with the given group name.
func (h *traceContextHandler) WithGroup(name string) slog.Handler {
	return &traceContextHandler{
		Handler:   h.Handler.WithGroup(name),
		subsystem: h.subsystem,
		level:     h.level,
	}
}

// multiHandler fans out log records to multiple handlers.
type multiHandler struct {
	handlers []slog.Handler
}

// Enabled returns true if any handler is enabled.
func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle writes the record to all handlers.
func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil {
				return err
			}
		}
	}
	return nil
}

// WithAttrs returns a new multiHandler with the given attributes.
func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: handlers}
}

// WithGroup returns a new multiHandler with the given group name.
func (m *multiHandler) WithGroup(name string) slog.Handler {
	handlers := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		handlers[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: handlers}
}

// AddToContext adds a logger to the context.
func AddToContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// FromContext retrieves the logger from context, or returns default.
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

// With returns a logger with additional attributes.
func With(logger *slog.Logger, args ...any) *slog.Logger {
	return logger.With(args...)
}
