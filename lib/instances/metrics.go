package instances

import (
	"context"
	"strconv"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	mw "github.com/kernel/hypeman/lib/middleware"
	hypotel "github.com/kernel/hypeman/lib/otel"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

type snapshotCompressionSource string

const (
	snapshotCompressionSourceStandby  snapshotCompressionSource = "standby"
	snapshotCompressionSourceSnapshot snapshotCompressionSource = "snapshot"
)

type snapshotCompressionResult string

const (
	snapshotCompressionResultSuccess  snapshotCompressionResult = "success"
	snapshotCompressionResultSkipped  snapshotCompressionResult = "skipped"
	snapshotCompressionResultCanceled snapshotCompressionResult = "canceled"
	snapshotCompressionResultFailed   snapshotCompressionResult = "failed"
)

type snapshotCompressionWaitOutcome string

const (
	snapshotCompressionWaitOutcomeStarted snapshotCompressionWaitOutcome = "started"
	snapshotCompressionWaitOutcomeSkipped snapshotCompressionWaitOutcome = "skipped"
)

type snapshotMemoryPreparePath string

const (
	snapshotMemoryPreparePathRaw        snapshotMemoryPreparePath = "raw"
	snapshotMemoryPreparePathDecompress snapshotMemoryPreparePath = "decompress"
)

type snapshotCompressionPreemptionOperation string

const (
	snapshotCompressionPreemptionRestoreInstance snapshotCompressionPreemptionOperation = "restore_instance"
	snapshotCompressionPreemptionRestoreSnapshot snapshotCompressionPreemptionOperation = "restore_snapshot"
	snapshotCompressionPreemptionForkSnapshot    snapshotCompressionPreemptionOperation = "fork_snapshot"
	snapshotCompressionPreemptionCreateSnapshot  snapshotCompressionPreemptionOperation = "create_snapshot"
	snapshotCompressionPreemptionDeleteInstance  snapshotCompressionPreemptionOperation = "delete_instance"
	snapshotCompressionPreemptionDeleteSnapshot  snapshotCompressionPreemptionOperation = "delete_snapshot"
)

type snapshotCodecOperation string

const (
	snapshotCodecOperationCompress   snapshotCodecOperation = "compress"
	snapshotCodecOperationDecompress snapshotCodecOperation = "decompress"
)

type snapshotCodecFallbackReason string

const (
	snapshotCodecFallbackReasonMissingBinary snapshotCodecFallbackReason = "missing_binary"
	snapshotCodecFallbackReasonNotExecutable snapshotCodecFallbackReason = "not_executable"
)

type lifecycleEventDropReason string

const lifecycleEventDropReasonBufferFull lifecycleEventDropReason = "buffer_full"

// Metrics holds the metrics instruments for instance operations.
type Metrics struct {
	createDuration                       metric.Float64Histogram
	restoreDuration                      metric.Float64Histogram
	standbyDuration                      metric.Float64Histogram
	stopDuration                         metric.Float64Histogram
	startDuration                        metric.Float64Histogram
	timeToRunning                        metric.Float64Histogram
	stateTransitions                     metric.Int64Counter
	snapshotCompressionJobsTotal         metric.Int64Counter
	snapshotCompressionDuration          metric.Float64Histogram
	snapshotCompressionWaitDuration      metric.Float64Histogram
	snapshotCompressionSavedBytes        metric.Int64Histogram
	snapshotCompressionRatio             metric.Float64Histogram
	snapshotCodecFallbacksTotal          metric.Int64Counter
	snapshotRestoreMemoryPrepareTotal    metric.Int64Counter
	snapshotRestoreMemoryPrepareDuration metric.Float64Histogram
	snapshotCompressionPreemptionsTotal  metric.Int64Counter
	lifecycleEventsDroppedTotal          metric.Int64Counter
	tracer                               trace.Tracer
}

// newInstanceMetrics creates and registers all instance metrics.
func newInstanceMetrics(meter metric.Meter, tracer trace.Tracer, m *manager) (*Metrics, error) {
	createDuration, err := meter.Float64Histogram(
		"hypeman_instances_create_duration_seconds",
		metric.WithDescription("Time to create an instance"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	restoreDuration, err := meter.Float64Histogram(
		"hypeman_instances_restore_duration_seconds",
		metric.WithDescription("Time to restore an instance from standby"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	standbyDuration, err := meter.Float64Histogram(
		"hypeman_instances_standby_duration_seconds",
		metric.WithDescription("Time to put an instance in standby"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	stopDuration, err := meter.Float64Histogram(
		"hypeman_instances_stop_duration_seconds",
		metric.WithDescription("Time to stop an instance"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	startDuration, err := meter.Float64Histogram(
		"hypeman_instances_start_duration_seconds",
		metric.WithDescription("Time to start an instance"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	timeToRunning, err := meter.Float64Histogram(
		"hypeman_instances_time_to_running_seconds",
		metric.WithDescription("Time from boot start until an instance reaches Running"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
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

	snapshotCompressionJobsTotal, err := meter.Int64Counter(
		"hypeman_snapshot_compression_jobs_total",
		metric.WithDescription("Total number of snapshot compression jobs by result"),
	)
	if err != nil {
		return nil, err
	}

	snapshotCompressionDuration, err := meter.Float64Histogram(
		"hypeman_snapshot_compression_duration_seconds",
		metric.WithDescription("Time to asynchronously compress snapshot memory"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	snapshotCompressionWaitDuration, err := meter.Float64Histogram(
		"hypeman_snapshot_compression_wait_duration_seconds",
		metric.WithDescription("Time a delayed snapshot compression job waits before compression starts or is skipped"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	snapshotCompressionSavedBytes, err := meter.Int64Histogram(
		"hypeman_snapshot_compression_saved_bytes",
		metric.WithDescription("Bytes saved by compressing snapshot memory"),
		metric.WithUnit("By"),
	)
	if err != nil {
		return nil, err
	}

	snapshotCompressionRatio, err := meter.Float64Histogram(
		"hypeman_snapshot_compression_ratio",
		metric.WithDescription("Compressed snapshot memory size divided by raw snapshot memory size"),
	)
	if err != nil {
		return nil, err
	}

	snapshotCodecFallbacksTotal, err := meter.Int64Counter(
		"hypeman_snapshot_codec_fallbacks_total",
		metric.WithDescription("Total number of snapshot codec fallbacks from native binaries to the Go implementation"),
	)
	if err != nil {
		return nil, err
	}

	snapshotRestoreMemoryPrepareTotal, err := meter.Int64Counter(
		"hypeman_snapshot_restore_memory_prepare_total",
		metric.WithDescription("Total number of snapshot memory prepare operations before restore"),
	)
	if err != nil {
		return nil, err
	}

	snapshotRestoreMemoryPrepareDuration, err := meter.Float64Histogram(
		"hypeman_snapshot_restore_memory_prepare_duration_seconds",
		metric.WithDescription("Time to prepare snapshot memory before restore"),
		metric.WithUnit("s"),
		metric.WithExplicitBucketBoundaries(hypotel.CommonDurationHistogramBuckets()...),
	)
	if err != nil {
		return nil, err
	}

	snapshotCompressionPreemptionsTotal, err := meter.Int64Counter(
		"hypeman_snapshot_compression_preemptions_total",
		metric.WithDescription("Total number of foreground operations that preempt in-flight snapshot compression"),
	)
	if err != nil {
		return nil, err
	}

	lifecycleEventsDroppedTotal, err := meter.Int64Counter(
		"hypeman_instances_lifecycle_events_dropped_total",
		metric.WithDescription("Total number of lifecycle events dropped because subscriber buffers were full"),
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

	oldestInStateSeconds, err := meter.Float64ObservableGauge(
		"hypeman_instances_oldest_in_state_seconds",
		metric.WithDescription("Age in seconds since creation of the oldest instance currently in each state"),
		metric.WithUnit("s"),
	)
	if err != nil {
		return nil, err
	}

	snapshotCompressionActiveTotal, err := meter.Int64ObservableGauge(
		"hypeman_snapshot_compression_active_total",
		metric.WithDescription("Total number of actively running snapshot compression jobs"),
	)
	if err != nil {
		return nil, err
	}

	snapshotCompressionPendingTotal, err := meter.Int64ObservableGauge(
		"hypeman_snapshot_compression_pending_total",
		metric.WithDescription("Total number of delayed snapshot compression jobs waiting to start"),
	)
	if err != nil {
		return nil, err
	}

	lifecycleSubscribersTotal, err := meter.Int64ObservableGauge(
		"hypeman_instances_lifecycle_subscribers_total",
		metric.WithDescription("Current number of lifecycle event subscribers by consumer"),
	)
	if err != nil {
		return nil, err
	}

	lifecycleSubscriberQueueDepth, err := meter.Int64ObservableGauge(
		"hypeman_instances_lifecycle_subscriber_queue_depth",
		metric.WithDescription("Maximum buffered lifecycle events across subscribers for each consumer"),
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
			oldestAgeSeconds := make(map[stateHypervisor]float64)
			now := m.nowUTC()
			for _, inst := range instances {
				key := stateHypervisor{
					state:      string(inst.State),
					hypervisor: string(inst.HypervisorType),
				}
				counts[key]++
				if inst.CreatedAt.IsZero() {
					continue
				}
				ageSeconds := now.Sub(inst.CreatedAt.UTC()).Seconds()
				if ageSeconds < 0 {
					ageSeconds = 0
				}
				if ageSeconds > oldestAgeSeconds[key] {
					oldestAgeSeconds[key] = ageSeconds
				}
			}
			for key, count := range counts {
				attrs := []attribute.KeyValue{
					attribute.String("state", key.state),
					attribute.String("hypervisor", key.hypervisor),
				}
				o.ObserveInt64(instancesTotal, count, metric.WithAttributes(attrs...))
				o.ObserveFloat64(oldestInStateSeconds, oldestAgeSeconds[key], metric.WithAttributes(attrs...))
			}
			return nil
		},
		instancesTotal,
		oldestInStateSeconds,
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			type compressionKey struct {
				hypervisor string
				algorithm  string
				source     string
			}

			activeCounts := make(map[compressionKey]int64)
			pendingCounts := make(map[compressionKey]int64)
			m.compressionMu.Lock()
			for _, job := range m.compressionJobs {
				key := compressionKey{
					hypervisor: string(job.target.HypervisorType),
					algorithm:  string(job.target.Policy.Algorithm),
					source:     string(job.target.Source),
				}
				switch job.state {
				case compressionJobStatePendingDelay:
					pendingCounts[key]++
				case compressionJobStateRunning:
					activeCounts[key]++
				}
			}
			m.compressionMu.Unlock()

			for key, count := range activeCounts {
				attrs := []attribute.KeyValue{
					attribute.String("algorithm", key.algorithm),
					attribute.String("source", key.source),
				}
				if key.hypervisor != "" {
					attrs = append(attrs, attribute.String("hypervisor", key.hypervisor))
				}
				o.ObserveInt64(snapshotCompressionActiveTotal, count, metric.WithAttributes(attrs...))
			}
			for key, count := range pendingCounts {
				attrs := []attribute.KeyValue{
					attribute.String("algorithm", key.algorithm),
					attribute.String("source", key.source),
				}
				if key.hypervisor != "" {
					attrs = append(attrs, attribute.String("hypervisor", key.hypervisor))
				}
				o.ObserveInt64(snapshotCompressionPendingTotal, count, metric.WithAttributes(attrs...))
			}
			return nil
		},
		snapshotCompressionActiveTotal,
		snapshotCompressionPendingTotal,
	)
	if err != nil {
		return nil, err
	}

	_, err = meter.RegisterCallback(
		func(ctx context.Context, o metric.Observer) error {
			stats := make(map[LifecycleEventConsumer]lifecycleConsumerStats, len(allLifecycleEventConsumers))
			if m.lifecycleEvents != nil {
				stats = m.lifecycleEvents.Stats()
			}
			for _, consumer := range allLifecycleEventConsumers {
				stat := stats[consumer]
				attrs := metric.WithAttributes(attribute.String("consumer", string(consumer)))
				o.ObserveInt64(lifecycleSubscribersTotal, stat.Subscribers, attrs)
				o.ObserveInt64(lifecycleSubscriberQueueDepth, stat.MaxQueueDepth, attrs)
			}
			return nil
		},
		lifecycleSubscribersTotal,
		lifecycleSubscriberQueueDepth,
	)
	if err != nil {
		return nil, err
	}

	return &Metrics{
		createDuration:                       createDuration,
		restoreDuration:                      restoreDuration,
		standbyDuration:                      standbyDuration,
		stopDuration:                         stopDuration,
		startDuration:                        startDuration,
		timeToRunning:                        timeToRunning,
		stateTransitions:                     stateTransitions,
		snapshotCompressionJobsTotal:         snapshotCompressionJobsTotal,
		snapshotCompressionDuration:          snapshotCompressionDuration,
		snapshotCompressionWaitDuration:      snapshotCompressionWaitDuration,
		snapshotCompressionSavedBytes:        snapshotCompressionSavedBytes,
		snapshotCompressionRatio:             snapshotCompressionRatio,
		snapshotCodecFallbacksTotal:          snapshotCodecFallbacksTotal,
		snapshotRestoreMemoryPrepareTotal:    snapshotRestoreMemoryPrepareTotal,
		snapshotRestoreMemoryPrepareDuration: snapshotRestoreMemoryPrepareDuration,
		snapshotCompressionPreemptionsTotal:  snapshotCompressionPreemptionsTotal,
		lifecycleEventsDroppedTotal:          lifecycleEventsDroppedTotal,
		tracer:                               tracer,
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

func compressionMetricAttributes(cfg *snapshotstore.SnapshotCompressionConfig) []attribute.KeyValue {
	algorithm := "none"
	level := "none"
	if cfg != nil && cfg.Enabled {
		if cfg.Algorithm != "" {
			algorithm = string(cfg.Algorithm)
		} else {
			algorithm = "unknown"
		}
		if cfg.Level != nil {
			level = strconv.Itoa(*cfg.Level)
		} else {
			level = "unknown"
		}
	}
	return []attribute.KeyValue{
		attribute.String("algorithm", algorithm),
		attribute.String("level", level),
	}
}

func (m *manager) recordDurationWithCompression(ctx context.Context, histogram metric.Float64Histogram, start time.Time, status string, hvType hypervisor.Type, compression *snapshotstore.SnapshotCompressionConfig) {
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
	attrs = append(attrs, compressionMetricAttributes(compression)...)
	histogram.Record(ctx, duration, metric.WithAttributes(attrs...))
}

func timeToRunningReadyAt(stored *StoredMetadata) *time.Time {
	if stored == nil || stored.ProgramStartedAt == nil {
		return nil
	}
	if stored.SkipGuestAgent || stored.GuestAgentReadyAt == nil {
		return stored.ProgramStartedAt
	}
	if stored.GuestAgentReadyAt.After(*stored.ProgramStartedAt) {
		return stored.GuestAgentReadyAt
	}
	return stored.ProgramStartedAt
}

func (m *manager) recordTimeToRunning(ctx context.Context, stored *StoredMetadata) {
	if m.metrics == nil || stored == nil || stored.StartedAt == nil {
		return
	}

	readyAt := timeToRunningReadyAt(stored)
	if readyAt == nil {
		return
	}

	duration := readyAt.UTC().Sub(stored.StartedAt.UTC()).Seconds()
	if duration < 0 {
		duration = 0
	}

	attrs := []attribute.KeyValue{}
	if stored.HypervisorType != "" {
		attrs = append(attrs, attribute.String("hypervisor", string(stored.HypervisorType)))
	}
	m.metrics.timeToRunning.Record(ctx, duration, metric.WithAttributes(attrs...))
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

func snapshotCompressionAttributes(hvType hypervisor.Type, algorithm snapshotstore.SnapshotCompressionAlgorithm, source snapshotCompressionSource) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("algorithm", string(algorithm)),
		attribute.String("source", string(source)),
	}
	if hvType != "" {
		attrs = append(attrs, attribute.String("hypervisor", string(hvType)))
	}
	return attrs
}

func (m *manager) recordSnapshotCompressionJob(ctx context.Context, target compressionTarget, result snapshotCompressionResult, compressionStart *time.Time, uncompressedSize, compressedSize int64) {
	if m.metrics == nil {
		return
	}

	attrs := snapshotCompressionAttributes(target.HypervisorType, target.Policy.Algorithm, target.Source)
	attrsWithResult := append([]attribute.KeyValue{}, attrs...)
	attrsWithResult = append(attrsWithResult, attribute.String("result", string(result)))

	m.metrics.snapshotCompressionJobsTotal.Add(ctx, 1, metric.WithAttributes(attrsWithResult...))
	if compressionStart != nil {
		m.metrics.snapshotCompressionDuration.Record(ctx, time.Since(*compressionStart).Seconds(), metric.WithAttributes(attrsWithResult...))
	}

	if result != snapshotCompressionResultSuccess || uncompressedSize <= 0 || compressedSize < 0 {
		return
	}

	savedBytes := uncompressedSize - compressedSize
	if savedBytes < 0 {
		savedBytes = 0
	}
	m.metrics.snapshotCompressionSavedBytes.Record(ctx, savedBytes, metric.WithAttributes(attrs...))
	m.metrics.snapshotCompressionRatio.Record(ctx, float64(compressedSize)/float64(uncompressedSize), metric.WithAttributes(attrs...))
}

func (m *manager) recordSnapshotCompressionWait(ctx context.Context, target compressionTarget, outcome snapshotCompressionWaitOutcome, start time.Time) {
	if m.metrics == nil {
		return
	}

	attrs := snapshotCompressionAttributes(target.HypervisorType, target.Policy.Algorithm, target.Source)
	attrs = append(attrs, attribute.String("outcome", string(outcome)))
	m.metrics.snapshotCompressionWaitDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

func (m *manager) recordSnapshotRestoreMemoryPrepare(ctx context.Context, hvType hypervisor.Type, path snapshotMemoryPreparePath, result snapshotCompressionResult, start time.Time) {
	if m.metrics == nil {
		return
	}

	attrs := []attribute.KeyValue{
		attribute.String("restore_source", string(path)),
		attribute.String("result", string(result)),
	}
	if hvType != "" {
		attrs = append(attrs, attribute.String("hypervisor", string(hvType)))
	}
	m.metrics.snapshotRestoreMemoryPrepareTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
	m.metrics.snapshotRestoreMemoryPrepareDuration.Record(ctx, time.Since(start).Seconds(), metric.WithAttributes(attrs...))
}

func (m *manager) recordSnapshotCompressionPreemption(ctx context.Context, operation snapshotCompressionPreemptionOperation, target compressionTarget) {
	if m.metrics == nil {
		return
	}

	attrs := snapshotCompressionAttributes(target.HypervisorType, target.Policy.Algorithm, target.Source)
	attrs = append(attrs, attribute.String("operation", string(operation)))
	m.metrics.snapshotCompressionPreemptionsTotal.Add(ctx, 1, metric.WithAttributes(attrs...))
}

func (m *manager) recordSnapshotCodecFallback(ctx context.Context, algorithm snapshotstore.SnapshotCompressionAlgorithm, operation snapshotCodecOperation, reason snapshotCodecFallbackReason) {
	if m.metrics == nil {
		return
	}

	m.metrics.snapshotCodecFallbacksTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("algorithm", string(algorithm)),
		attribute.String("operation", string(operation)),
		attribute.String("reason", string(reason)),
	))
}

func (m *manager) recordLifecycleEventDropped(ctx context.Context, consumer LifecycleEventConsumer, reason lifecycleEventDropReason) {
	if m.metrics == nil {
		return
	}

	m.metrics.lifecycleEventsDroppedTotal.Add(ctx, 1, metric.WithAttributes(
		attribute.String("consumer", string(consumer)),
		attribute.String("reason", string(reason)),
	))
}
