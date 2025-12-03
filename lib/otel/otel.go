// Package otel provides OpenTelemetry initialization and configuration.
package otel

import (
	"context"
	"fmt"
	goruntime "runtime"
	"time"

	otelruntime "go.opentelemetry.io/contrib/instrumentation/runtime"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.24.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Config holds OpenTelemetry configuration.
type Config struct {
	Enabled     bool
	Endpoint    string
	ServiceName string
	Insecure    bool
	Version     string
}

// Provider holds initialized OTel providers.
type Provider struct {
	TracerProvider *sdktrace.TracerProvider
	MeterProvider  *sdkmetric.MeterProvider
	Tracer         trace.Tracer
	Meter          metric.Meter
	startTime      time.Time
}

// Init initializes OpenTelemetry with the given configuration.
// Returns a shutdown function that should be called on application exit.
// If OTel is disabled, returns a no-op shutdown function.
func Init(ctx context.Context, cfg Config) (*Provider, func(context.Context) error, error) {
	if !cfg.Enabled {
		// Return no-op provider when disabled
		return &Provider{
			Tracer:    otel.Tracer(cfg.ServiceName),
			Meter:     otel.Meter(cfg.ServiceName),
			startTime: time.Now(),
		}, func(context.Context) error { return nil }, nil
	}

	// Create resource with service information
	res, err := resource.Merge(
		resource.Default(),
		resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.Version),
		),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create resource: %w", err)
	}

	// Setup gRPC connection options
	var dialOpts []grpc.DialOption
	if cfg.Insecure {
		dialOpts = append(dialOpts, grpc.WithTransportCredentials(insecure.NewCredentials()))
	}

	// Create trace exporter
	traceExporter, err := otlptracegrpc.New(ctx,
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
		otlptracegrpc.WithDialOption(dialOpts...),
	)
	if err != nil {
		return nil, nil, fmt.Errorf("create trace exporter: %w", err)
	}

	// Create tracer provider
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(traceExporter),
		sdktrace.WithResource(res),
	)

	// Create metric exporter
	metricExporter, err := otlpmetricgrpc.New(ctx,
		otlpmetricgrpc.WithEndpoint(cfg.Endpoint),
		otlpmetricgrpc.WithDialOption(dialOpts...),
	)
	if err != nil {
		tracerProvider.Shutdown(ctx)
		return nil, nil, fmt.Errorf("create metric exporter: %w", err)
	}

	// Create meter provider
	meterProvider := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(metricExporter)),
		sdkmetric.WithResource(res),
	)

	// Set global providers
	otel.SetTracerProvider(tracerProvider)
	otel.SetMeterProvider(meterProvider)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	// Start runtime metrics collection
	if err := otelruntime.Start(otelruntime.WithMeterProvider(meterProvider)); err != nil {
		tracerProvider.Shutdown(ctx)
		meterProvider.Shutdown(ctx)
		return nil, nil, fmt.Errorf("start runtime metrics: %w", err)
	}

	provider := &Provider{
		TracerProvider: tracerProvider,
		MeterProvider:  meterProvider,
		Tracer:         tracerProvider.Tracer(cfg.ServiceName),
		Meter:          meterProvider.Meter(cfg.ServiceName),
		startTime:      time.Now(),
	}

	// Register system metrics (uptime, info)
	if err := provider.registerSystemMetrics(cfg); err != nil {
		tracerProvider.Shutdown(ctx)
		meterProvider.Shutdown(ctx)
		return nil, nil, fmt.Errorf("register system metrics: %w", err)
	}

	shutdown := func(ctx context.Context) error {
		var errs []error
		if err := tracerProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown tracer: %w", err))
		}
		if err := meterProvider.Shutdown(ctx); err != nil {
			errs = append(errs, fmt.Errorf("shutdown meter: %w", err))
		}
		if len(errs) > 0 {
			return fmt.Errorf("shutdown errors: %v", errs)
		}
		return nil
	}

	return provider, shutdown, nil
}

// registerSystemMetrics registers uptime and info metrics.
func (p *Provider) registerSystemMetrics(cfg Config) error {
	// Uptime gauge
	uptime, err := p.Meter.Float64ObservableGauge(
		"hypeman_uptime_seconds",
		metric.WithDescription("Process uptime in seconds"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return fmt.Errorf("create uptime gauge: %w", err)
	}

	// Info gauge (always 1, with version labels)
	info, err := p.Meter.Int64ObservableGauge(
		"hypeman_info",
		metric.WithDescription("Hypeman build information"),
	)
	if err != nil {
		return fmt.Errorf("create info gauge: %w", err)
	}

	_, err = p.Meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			o.ObserveFloat64(uptime, time.Since(p.startTime).Seconds())
			o.ObserveInt64(info, 1,
				metric.WithAttributes(
					semconv.ServiceVersion(cfg.Version),
					semconv.TelemetrySDKLanguageGo,
				),
			)
			return nil
		},
		uptime,
		info,
	)
	if err != nil {
		return fmt.Errorf("register callback: %w", err)
	}

	return nil
}

// Tracer returns a tracer for the given subsystem.
func (p *Provider) TracerFor(subsystem string) trace.Tracer {
	if p.TracerProvider != nil {
		return p.TracerProvider.Tracer(subsystem)
	}
	return otel.Tracer(subsystem)
}

// Meter returns a meter for the given subsystem.
func (p *Provider) MeterFor(subsystem string) metric.Meter {
	if p.MeterProvider != nil {
		return p.MeterProvider.Meter(subsystem)
	}
	return otel.Meter(subsystem)
}

// GoVersion returns the Go version used to build the binary.
func GoVersion() string {
	return goruntime.Version()
}
