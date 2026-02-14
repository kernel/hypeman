package guest

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for guest operations.
type Metrics struct {
	execSessionsTotal      metric.Int64Counter
	execDuration           metric.Float64Histogram
	execBytesSentTotal     metric.Int64Counter
	execBytesReceivedTotal metric.Int64Counter

	cpSessionsTotal metric.Int64Counter
	cpDuration      metric.Float64Histogram
	cpBytesTotal    metric.Int64Counter
}

// GuestMetrics is the global metrics instance for the guest package.
// Set this via SetMetrics() during application initialization.
var GuestMetrics *Metrics

// SetMetrics sets the global metrics instance.
func SetMetrics(m *Metrics) {
	GuestMetrics = m
}

// NewMetrics creates guest metrics instruments.
// If meter is nil, returns nil (metrics disabled).
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	if meter == nil {
		return nil, nil
	}

	execSessionsTotal, err := meter.Int64Counter(
		"hypeman_exec_sessions_total",
		metric.WithDescription("Total number of exec sessions"),
	)
	if err != nil {
		return nil, err
	}

	execDuration, err := meter.Float64Histogram(
		"hypeman_exec_duration_seconds",
		metric.WithDescription("Exec command duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	execBytesSentTotal, err := meter.Int64Counter(
		"hypeman_exec_bytes_sent_total",
		metric.WithDescription("Total bytes sent to guest (stdin)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	execBytesReceivedTotal, err := meter.Int64Counter(
		"hypeman_exec_bytes_received_total",
		metric.WithDescription("Total bytes received from guest (stdout+stderr)"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	cpSessionsTotal, err := meter.Int64Counter(
		"hypeman_cp_sessions_total",
		metric.WithDescription("Total number of cp (copy) sessions"),
	)
	if err != nil {
		return nil, err
	}

	cpDuration, err := meter.Float64Histogram(
		"hypeman_cp_duration_seconds",
		metric.WithDescription("Copy operation duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	cpBytesTotal, err := meter.Int64Counter(
		"hypeman_cp_bytes_total",
		metric.WithDescription("Total bytes transferred during copy operations"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		execSessionsTotal:      execSessionsTotal,
		execDuration:           execDuration,
		execBytesSentTotal:     execBytesSentTotal,
		execBytesReceivedTotal: execBytesReceivedTotal,
		cpSessionsTotal:        cpSessionsTotal,
		cpDuration:             cpDuration,
		cpBytesTotal:           cpBytesTotal,
	}, nil
}

// RecordExecSession records metrics for a completed exec session.
func (m *Metrics) RecordExecSession(ctx context.Context, start time.Time, exitCode int, bytesSent, bytesReceived int64) {
	if m == nil {
		return
	}

	duration := time.Since(start).Seconds()
	status := "success"
	if exitCode != 0 {
		status = "error"
	}

	m.execSessionsTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("status", status),
			attribute.Int("exit_code", exitCode),
		))

	m.execDuration.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))

	if bytesSent > 0 {
		m.execBytesSentTotal.Add(ctx, bytesSent)
	}
	if bytesReceived > 0 {
		m.execBytesReceivedTotal.Add(ctx, bytesReceived)
	}
}

// RecordCpSession records metrics for a completed cp (copy) session.
// direction should be "to" (copy to instance) or "from" (copy from instance).
func (m *Metrics) RecordCpSession(ctx context.Context, start time.Time, direction string, success bool, bytesTransferred int64) {
	if m == nil {
		return
	}

	duration := time.Since(start).Seconds()
	status := "success"
	if !success {
		status = "error"
	}

	m.cpSessionsTotal.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("direction", direction),
			attribute.String("status", status),
		))

	m.cpDuration.Record(ctx, duration,
		metric.WithAttributes(
			attribute.String("direction", direction),
			attribute.String("status", status),
		))

	if bytesTransferred > 0 {
		m.cpBytesTotal.Add(ctx, bytesTransferred,
			metric.WithAttributes(
				attribute.String("direction", direction),
			))
	}
}

