package logger

import (
	"context"
	"log/slog"
)

type contextKey string

const loggerKey contextKey = "logger"

// AddToContext adds a logger to the context
func AddToContext(ctx context.Context, logger *slog.Logger) context.Context {
	return context.WithValue(ctx, loggerKey, logger)
}

// FromContext retrieves the logger from context, or returns default
func FromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey).(*slog.Logger); ok {
		return logger
	}
	return slog.Default()
}

