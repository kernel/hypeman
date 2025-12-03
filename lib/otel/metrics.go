package otel

import (
	"go.opentelemetry.io/otel/metric"
)

// ImageMetrics holds metrics for the image manager.
type ImageMetrics struct {
	BuildQueueLength metric.Int64ObservableGauge
	BuildDuration    metric.Float64Histogram
	ImagesTotal      metric.Int64ObservableGauge
	PullsTotal       metric.Int64Counter
}

// NewImageMetrics creates metrics for the image manager.
func NewImageMetrics(meter metric.Meter) (*ImageMetrics, error) {
	buildQueueLength, err := meter.Int64ObservableGauge(
		"hypeman_images_build_queue_length",
		metric.WithDescription("Current number of images in the build queue"),
	)
	if err != nil {
		return nil, err
	}

	buildDuration, err := meter.Float64Histogram(
		"hypeman_images_build_duration_seconds",
		metric.WithDescription("Time to build an image"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	imagesTotal, err := meter.Int64ObservableGauge(
		"hypeman_images_total",
		metric.WithDescription("Total number of cached images"),
	)
	if err != nil {
		return nil, err
	}

	pullsTotal, err := meter.Int64Counter(
		"hypeman_images_pulls_total",
		metric.WithDescription("Total number of image pulls from registries"),
	)
	if err != nil {
		return nil, err
	}

	return &ImageMetrics{
		BuildQueueLength: buildQueueLength,
		BuildDuration:    buildDuration,
		ImagesTotal:      imagesTotal,
		PullsTotal:       pullsTotal,
	}, nil
}

// InstanceMetrics holds metrics for the instance manager.
type InstanceMetrics struct {
	InstancesTotal   metric.Int64ObservableGauge
	CreateDuration   metric.Float64Histogram
	RestoreDuration  metric.Float64Histogram
	StandbyDuration  metric.Float64Histogram
	StateTransitions metric.Int64Counter
}

// NewInstanceMetrics creates metrics for the instance manager.
func NewInstanceMetrics(meter metric.Meter) (*InstanceMetrics, error) {
	instancesTotal, err := meter.Int64ObservableGauge(
		"hypeman_instances_total",
		metric.WithDescription("Total number of instances by state"),
	)
	if err != nil {
		return nil, err
	}

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

	return &InstanceMetrics{
		InstancesTotal:   instancesTotal,
		CreateDuration:   createDuration,
		RestoreDuration:  restoreDuration,
		StandbyDuration:  standbyDuration,
		StateTransitions: stateTransitions,
	}, nil
}

// NetworkMetrics holds metrics for the network manager.
type NetworkMetrics struct {
	AllocationsTotal metric.Int64ObservableGauge
	TapOperations    metric.Int64Counter
}

// NewNetworkMetrics creates metrics for the network manager.
func NewNetworkMetrics(meter metric.Meter) (*NetworkMetrics, error) {
	allocationsTotal, err := meter.Int64ObservableGauge(
		"hypeman_network_allocations_total",
		metric.WithDescription("Total number of active network allocations"),
	)
	if err != nil {
		return nil, err
	}

	tapOperations, err := meter.Int64Counter(
		"hypeman_network_tap_operations_total",
		metric.WithDescription("Total number of TAP device operations"),
	)
	if err != nil {
		return nil, err
	}

	return &NetworkMetrics{
		AllocationsTotal: allocationsTotal,
		TapOperations:    tapOperations,
	}, nil
}

// VolumeMetrics holds metrics for the volume manager.
type VolumeMetrics struct {
	VolumesTotal   metric.Int64ObservableGauge
	AllocatedBytes metric.Int64ObservableGauge
	UsedBytes      metric.Int64ObservableGauge
	CreateDuration metric.Float64Histogram
}

// NewVolumeMetrics creates metrics for the volume manager.
func NewVolumeMetrics(meter metric.Meter) (*VolumeMetrics, error) {
	volumesTotal, err := meter.Int64ObservableGauge(
		"hypeman_volumes_total",
		metric.WithDescription("Total number of volumes"),
	)
	if err != nil {
		return nil, err
	}

	allocatedBytes, err := meter.Int64ObservableGauge(
		"hypeman_volumes_allocated_bytes",
		metric.WithDescription("Total allocated/provisioned volume size in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	usedBytes, err := meter.Int64ObservableGauge(
		"hypeman_volumes_used_bytes",
		metric.WithDescription("Actual disk space consumed by volumes in bytes"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	createDuration, err := meter.Float64Histogram(
		"hypeman_volumes_create_duration_seconds",
		metric.WithDescription("Time to create a volume"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	return &VolumeMetrics{
		VolumesTotal:   volumesTotal,
		AllocatedBytes: allocatedBytes,
		UsedBytes:      usedBytes,
		CreateDuration: createDuration,
	}, nil
}

// ExecMetrics holds metrics for the exec client.
type ExecMetrics struct {
	SessionsTotal      metric.Int64Counter
	Duration           metric.Float64Histogram
	BytesSentTotal     metric.Int64Counter
	BytesReceivedTotal metric.Int64Counter
}

// NewExecMetrics creates metrics for the exec client.
func NewExecMetrics(meter metric.Meter) (*ExecMetrics, error) {
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

	return &ExecMetrics{
		SessionsTotal:      sessionsTotal,
		Duration:           duration,
		BytesSentTotal:     bytesSentTotal,
		BytesReceivedTotal: bytesReceivedTotal,
	}, nil
}

// VMMMetrics holds metrics for the VMM client.
type VMMMetrics struct {
	APIDuration    metric.Float64Histogram
	APIErrorsTotal metric.Int64Counter
}

// NewVMMMetrics creates metrics for the VMM client.
func NewVMMMetrics(meter metric.Meter) (*VMMMetrics, error) {
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

	return &VMMMetrics{
		APIDuration:    apiDuration,
		APIErrorsTotal: apiErrorsTotal,
	}, nil
}

// HTTPMetrics holds metrics for HTTP middleware.
type HTTPMetrics struct {
	RequestsTotal   metric.Int64Counter
	RequestDuration metric.Float64Histogram
}

// NewHTTPMetrics creates metrics for HTTP middleware.
func NewHTTPMetrics(meter metric.Meter) (*HTTPMetrics, error) {
	requestsTotal, err := meter.Int64Counter(
		"hypeman_http_requests_total",
		metric.WithDescription("Total number of HTTP requests"),
	)
	if err != nil {
		return nil, err
	}

	requestDuration, err := meter.Float64Histogram(
		"hypeman_http_request_duration_seconds",
		metric.WithDescription("HTTP request duration"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	return &HTTPMetrics{
		RequestsTotal:   requestsTotal,
		RequestDuration: requestDuration,
	}, nil
}
