package instances

import (
	"context"
	"time"

	"github.com/onkernel/hypeman/lib/hypervisor"
	mw "github.com/onkernel/hypeman/lib/middleware"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// Metrics holds the metrics instruments for instance operations.
type Metrics struct {
	createDuration   metric.Float64Histogram
	restoreDuration  metric.Float64Histogram
	standbyDuration  metric.Float64Histogram
	stopDuration     metric.Float64Histogram
	startDuration    metric.Float64Histogram
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

	stopDuration, err := meter.Float64Histogram(
		"hypeman_instances_stop_duration_seconds",
		metric.WithDescription("Time to stop an instance"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	startDuration, err := meter.Float64Histogram(
		"hypeman_instances_start_duration_seconds",
		metric.WithDescription("Time to start an instance"),
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
			// Count by state and hypervisor combination
			type stateHypervisor struct {
				state      string
				hypervisor string
			}
			counts := make(map[stateHypervisor]int64)
			for _, inst := range instances {
				key := stateHypervisor{
					state:      string(inst.State),
					hypervisor: string(inst.HypervisorType),
				}
				counts[key]++
			}
			for key, count := range counts {
				o.ObserveInt64(instancesTotal, count,
					metric.WithAttributes(
						attribute.String("state", key.state),
						attribute.String("hypervisor", key.hypervisor),
					))
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
		stopDuration:     stopDuration,
		startDuration:    startDuration,
		stateTransitions: stateTransitions,
		tracer:           tracer,
	}, nil
}

// getHypervisorFromContext extracts the hypervisor type from the resolved instance in context.
// Returns empty string if not available.
func getHypervisorFromContext(ctx context.Context) string {
	if inst := mw.GetResolvedInstance[Instance](ctx); inst != nil {
		return string(inst.HypervisorType)
	}
	return ""
}

// recordDuration records operation duration with hypervisor label.
func (m *manager) recordDuration(ctx context.Context, histogram metric.Float64Histogram, start time.Time, status string, hvType hypervisor.Type) {
	if m.metrics == nil {
		return
	}
	duration := time.Since(start).Seconds()
	attrs := []attribute.KeyValue{
		attribute.String("status", status),
	}
	if hvType != "" {
		attrs = append(attrs, attribute.String("hypervisor", string(hvType)))
	}
	histogram.Record(ctx, duration, metric.WithAttributes(attrs...))
}

// recordStateTransition records a state transition with hypervisor label.
func (m *manager) recordStateTransition(ctx context.Context, fromState, toState string, hvType hypervisor.Type) {
	if m.metrics == nil {
		return
	}
	attrs := []attribute.KeyValue{
		attribute.String("from", fromState),
		attribute.String("to", toState),
	}
	if hvType != "" {
		attrs = append(attrs, attribute.String("hypervisor", string(hvType)))
	}
	m.metrics.stateTransitions.Add(ctx, 1, metric.WithAttributes(attrs...))
}
