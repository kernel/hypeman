package egressproxy

import (
	"context"

	hypotel "github.com/kernel/hypeman/lib/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

type metrics struct {
	registrations        metric.Int64Counter
	ruleUpdates          metric.Int64Counter
	registeredInstances  metric.Int64ObservableGauge
	controlPlaneDuration metric.Float64Histogram
	requests             metric.Int64Counter
	upstreamDuration     metric.Float64Histogram
	upstreamFailures     metric.Int64Counter
}

func newMetrics(meter metric.Meter, svc *Service) (*metrics, error) {
	registrations, err := meter.Int64Counter(
		"hypeman_egress_proxy_registrations_total",
		metric.WithDescription("Total number of egress proxy registration operations"),
	)
	if err != nil {
		return nil, err
	}

	ruleUpdates, err := meter.Int64Counter(
		"hypeman_egress_proxy_rule_updates_total",
		metric.WithDescription("Total number of egress proxy rule update operations"),
	)
	if err != nil {
		return nil, err
	}

	registeredInstances, err := meter.Int64ObservableGauge(
		"hypeman_egress_proxy_registered_instances_total",
		metric.WithDescription("Total number of instances currently registered with the egress proxy"),
	)
	if err != nil {
		return nil, err
	}

	controlPlaneDuration, err := meter.Float64Histogram(
		"hypeman_egress_proxy_control_plane_duration_seconds",
		metric.WithDescription("Duration of egress proxy control plane operations"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	requests, err := meter.Int64Counter(
		"hypeman_egress_proxy_requests_total",
		metric.WithDescription("Total number of egress proxy request handling outcomes"),
	)
	if err != nil {
		return nil, err
	}

	upstreamDuration, err := meter.Float64Histogram(
		"hypeman_egress_proxy_upstream_duration_seconds",
		metric.WithDescription("Duration of egress proxy upstream requests"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	upstreamFailures, err := meter.Int64Counter(
		"hypeman_egress_proxy_upstream_failures_total",
		metric.WithDescription("Total number of egress proxy upstream request failures"),
	)
	if err != nil {
		return nil, err
	}

	if _, err := meter.RegisterCallback(func(ctx context.Context, o metric.Observer) error {
		svc.mu.RLock()
		count := int64(len(svc.sourceIPByInstance))
		svc.mu.RUnlock()
		o.ObserveInt64(registeredInstances, count)
		return nil
	}, registeredInstances); err != nil {
		return nil, err
	}

	return &metrics{
		registrations:        registrations,
		ruleUpdates:          ruleUpdates,
		registeredInstances:  registeredInstances,
		controlPlaneDuration: controlPlaneDuration,
		requests:             requests,
		upstreamDuration:     upstreamDuration,
		upstreamFailures:     upstreamFailures,
	}, nil
}

func (m *metrics) recordRegistration(ctx context.Context, operation, result, enforcementMode string) {
	if m == nil {
		return
	}
	m.registrations.Add(ctx, 1, metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.String("result", result),
		attribute.String("enforcement_mode", enforcementMode),
	))
}

func (m *metrics) recordRuleUpdate(ctx context.Context, result string) {
	if m == nil {
		return
	}
	m.ruleUpdates.Add(ctx, 1, metric.WithAttributes(
		attribute.String("result", result),
	))
}

func (m *metrics) recordControlPlaneDuration(ctx context.Context, operation, result string, seconds float64) {
	if m == nil {
		return
	}
	m.controlPlaneDuration.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("operation", operation),
		attribute.String("result", result),
	))
}

func (m *metrics) recordRequest(ctx context.Context, protocol, result string, injected bool) {
	if m == nil {
		return
	}
	m.requests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("protocol", protocol),
		attribute.String("result", result),
		attribute.Bool("injected", injected),
	))
}

func (m *metrics) recordUpstreamDuration(ctx context.Context, protocol, result string, seconds float64) {
	if m == nil {
		return
	}
	m.upstreamDuration.Record(ctx, seconds, metric.WithAttributes(
		attribute.String("protocol", protocol),
		attribute.String("result", result),
	))
}

func (m *metrics) recordUpstreamFailure(ctx context.Context, protocol string) {
	if m == nil {
		return
	}
	m.upstreamFailures.Add(ctx, 1, metric.WithAttributes(
		attribute.String("protocol", protocol),
	))
}
