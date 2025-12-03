package instances

import (
	"context"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Metrics holds the metrics instruments for instance operations.
type Metrics struct {
	createDuration   metric.Float64Histogram
	restoreDuration  metric.Float64Histogram
	standbyDuration  metric.Float64Histogram
	stateTransitions metric.Int64Counter
	tracer           trace.Tracer
}

// newInstanceMetrics creates and registers all instance metrics.
func newInstanceMetrics(meter metric.Meter, tracer trace.Tracer, m *manager) (*Metrics, error) {
	createDuration, err := meter.Float64Histogram(
		"hypeman_instances_create_duration_seconds",
		metric.WithDescription("Time to create an instance"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	restoreDuration, err := meter.Float64Histogram(
		"hypeman_instances_restore_duration_seconds",
		metric.WithDescription("Time to restore an instance from standby"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	standbyDuration, err := meter.Float64Histogram(
		"hypeman_instances_standby_duration_seconds",
		metric.WithDescription("Time to put an instance in standby"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	stateTransitions, err := meter.Int64Counter(
		"hypeman_instances_state_transitions_total",
		metric.WithDescription("Total number of instance state transitions"),
	)
	if err != nil {
		return nil, err
	}

	// Register observable gauge for instance counts by state
	instancesTotal, err := meter.Int64ObservableGauge(
		"hypeman_instances_total",
		metric.WithDescription("Total number of instances by state"),
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			instances, err := m.listInstances(ctx)
			if err != nil {
				return nil
			}
			stateCounts := make(map[string]int64)
			for _, inst := range instances {
				stateCounts[string(inst.State)]++
			}
			for state, count := range stateCounts {
				o.ObserveInt64(instancesTotal, count,
					metric.WithAttributes(attribute.String("state", state)))
			}
			return nil
		},
		instancesTotal,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		createDuration:   createDuration,
		restoreDuration:  restoreDuration,
		standbyDuration:  standbyDuration,
		stateTransitions: stateTransitions,
		tracer:           tracer,
	}, nil
}

// recordDuration records operation duration.
func (m *manager) recordDuration(ctx context.Context, histogram metric.Float64Histogram, start time.Time, status string) {
	if m.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	histogram.Record(ctx, duration,
		metric.WithAttributes(attribute.String("status", status)))
}

// recordStateTransition records a state transition.
func (m *manager) recordStateTransition(ctx context.Context, fromState, toState string) {
	if m.metrics == nil {
		return
	}
	m.metrics.stateTransitions.Add(ctx, 1,
		metric.WithAttributes(
			attribute.String("from", fromState),
			attribute.String("to", toState),
		))
}
