package network

import (
	"context"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Metrics holds the metrics instruments for network operations.
type Metrics struct {
	tapOperations     metric.Int64Counter
	tcClassCollisions metric.Int64Counter
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

	bridgeHTBClassCount, err := meter.Int64ObservableGauge(
		"hypeman_network_bridge_htb_class_count",
		metric.WithDescription("Current number of non-root HTB classes on the network bridge"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			allocs, err := m.ListAllocations(ctx)
			if err == nil {
				o.ObserveInt64(allocationsTotal, int64(len(allocs)))
			}

			classCount, err := m.bridgeHTBClassCount(ctx)
			if err == nil {
				o.ObserveInt64(bridgeHTBClassCount, classCount)
			}
			return nil
		},
		allocationsTotal,
		bridgeHTBClassCount,
	)
	if err != nil {
		return nil, err
	}

	tcClassCollisions, err := meter.Int64Counter(
		"hypeman_network_tc_class_collisions_total",
		metric.WithDescription("Total number of tc class ID collisions during addVMClass"),
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		tapOperations:     tapOperations,
		tcClassCollisions: tcClassCollisions,
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

// recordTCClassCollision records a tc class ID collision.
// attempt is "initial" for the first hash collision or "retry" for subsequent probe collisions.
func (m *manager) recordTCClassCollision(ctx context.Context, attempt string) {
	if m.metrics == nil {
		return
	}
	m.metrics.tcClassCollisions.Add(ctx, 1,
		metric.WithAttributes(attribute.String("attempt", attempt)))
}
