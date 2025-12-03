package vmm

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for VMM operations.
type Metrics struct {
	APIDuration    metric.Float64Histogram
	APIErrorsTotal metric.Int64Counter
}

// NewMetrics creates VMM metrics instruments.
// If meter is nil, returns nil (metrics disabled).
func NewMetrics(meter metric.Meter) (*Metrics, error) {
	if meter == nil {
		return nil, nil
	}

	apiDuration, err := meter.Float64Histogram(
		"hypeman_vmm_api_duration_seconds",
		metric.WithDescription("Cloud Hypervisor API call duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	apiErrorsTotal, err := meter.Int64Counter(
		"hypeman_vmm_api_errors_total",
		metric.WithDescription("Total number of Cloud Hypervisor API errors"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		APIDuration:    apiDuration,
		APIErrorsTotal: apiErrorsTotal,
	}, nil
}

// RecordAPICall records the duration and status of an API call.
func (m *Metrics) RecordAPICall(ctx context.Context, operation string, start time.Time, err error) {
	if m == nil {
		return
	}

	duration := time.Since(start).Seconds()
	status := "success"
	if err != nil {
		status = "error"
		m.APIErrorsTotal.Add(ctx, 1,
			metric.WithAttributes(attribute.String("operation", operation)))
	}

	m.APIDuration.Record(ctx, duration,
		metric.WithAttributes(
			attribute.String("operation", operation),
			attribute.String("status", status),
		))
}
