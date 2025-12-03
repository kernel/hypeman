package network

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for network operations.
type Metrics struct {
	tapOperations metric.Int64Counter
}

// newNetworkMetrics creates and registers all network metrics.
func newNetworkMetrics(meter metric.Meter, m *manager) (*Metrics, error) {
	tapOperations, err := meter.Int64Counter(
		"hypeman_network_tap_operations_total",
		metric.WithDescription("Total number of TAP device operations"),
	)
	if err != nil {
		return nil, err
	}

	// Register observable gauge for allocations
	allocationsTotal, err := meter.Int64ObservableGauge(
		"hypeman_network_allocations_total",
		metric.WithDescription("Total number of active network allocations"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			allocs, err := m.ListAllocations(ctx)
			if err != nil {
				return nil
			}
			o.ObserveInt64(allocationsTotal, int64(len(allocs)))
			return nil
		},
		allocationsTotal,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		tapOperations: tapOperations,
	}, nil
}

// recordTAPOperation records a TAP device operation.
func (m *manager) recordTAPOperation(ctx context.Context, operation string) {
	if m.metrics == nil {
		return
	}
	m.metrics.tapOperations.Add(ctx, 1,
		metric.WithAttributes(attribute.String("operation", operation)))
}
