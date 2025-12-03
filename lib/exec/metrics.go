package exec

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for exec operations.
type Metrics struct {
	sessionsTotal      metric.Int64Counter
	duration           metric.Float64Histogram
	bytesSentTotal     metric.Int64Counter
	bytesReceivedTotal metric.Int64Counter
}

// ExecMetrics is the global metrics instance for the exec package.
// Set this via SetMetrics() during application initialization.
var ExecMetrics *Metrics

// SetMetrics sets the global metrics instance.
func SetMetrics(m *Metrics) {
	ExecMetrics = m
}

// NewMetrics creates exec metrics instruments.
// If meter is nil, returns nil (metrics disabled).
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	if meter == nil {
		return nil, nil
	}

	sessionsTotal, err := meter.Int64Counter(
		"hypeman_exec_sessions_total",
		metric.WithDescription("Total number of exec sessions"),
	)
	if err != nil {
		return nil, err
	}

	duration, err := meter.Float64Histogram(
		"hypeman_exec_duration_seconds",
		metric.WithDescription("Exec command duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	bytesSentTotal, err := meter.Int64Counter(
		"hypeman_exec_bytes_sent_total",
		metric.WithDescription("Total bytes sent to guest (stdin)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	bytesReceivedTotal, err := meter.Int64Counter(
		"hypeman_exec_bytes_received_total",
		metric.WithDescription("Total bytes received from guest (stdout+stderr)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		sessionsTotal:      sessionsTotal,
		duration:           duration,
		bytesSentTotal:     bytesSentTotal,
		bytesReceivedTotal: bytesReceivedTotal,
	}, nil
}

// RecordSession records metrics for a completed exec session.
func (m *Metrics) RecordSession(ctx context.Context, start time.Time, exitCode int, bytesSent, bytesReceived int64) {
	if m == nil {
		return
	}

	duration := time.Since(start).Seconds()
	status := "success"
	if exitCode != 0 {
		status = "error"
	}

	m.sessionsTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("status", status),
			attribute.Int("exit_code", exitCode),
		))

	m.duration.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))

	if bytesSent > 0 {
		m.bytesSentTotal.Add(ctx, bytesSent)
	}
	if bytesReceived > 0 {
		m.bytesReceivedTotal.Add(ctx, bytesReceived)
	}
}
