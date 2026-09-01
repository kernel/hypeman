package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/attribute"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestSnapshotCompressionMetrics_RecordAndObserve(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	m := &manager{
		paths: paths.New(t.TempDir()),
		compressionJobs: map[string]*compressionJob{
			"job-1": {
				done:  make(chan struct{}),
				state: compressionJobStateRunning,
				target: compressionTarget{
					Key:            "job-1",
					HypervisorType: hypervisor.TypeCloudHypervisor,
					Source:         snapshotCompressionSourceStandby,
					Policy: snapshotstore.SnapshotCompressionConfig{
						Enabled:   true,
						Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
					},
				},
			},
			"job-2": {
				done:  make(chan struct{}),
				state: compressionJobStatePendingDelay,
				target: compressionTarget{
					Key:            "job-2",
					HypervisorType: hypervisor.TypeQEMU,
					Source:         snapshotCompressionSourceStandby,
					Policy: snapshotstore.SnapshotCompressionConfig{
						Enabled:   true,
						Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
					},
				},
			},
		},
	}

	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics

	target := m.compressionJobs["job-1"].target
	startedAt := time.Now().Add(-2 * time.Second)
	m.recordSnapshotCompressionJob(t.Context(), target, snapshotCompressionResultSuccess, &startedAt, 1024, 256)
	m.recordSnapshotCompressionJob(t.Context(), m.compressionJobs["job-2"].target, snapshotCompressionResultSkipped, nil, 0, 0)
	m.recordSnapshotCompressionWait(t.Context(), m.compressionJobs["job-2"].target, snapshotCompressionWaitOutcomeSkipped, time.Now().Add(-1500*time.Millisecond))
	m.recordSnapshotCodecFallback(t.Context(), snapshotstore.SnapshotCompressionAlgorithmLz4, snapshotCodecOperationCompress, snapshotCodecFallbackReasonMissingBinary)
	m.recordSnapshotRestoreMemoryPrepare(t.Context(), hypervisor.TypeCloudHypervisor, snapshotMemoryPreparePathRaw, snapshotCompressionResultSuccess, time.Now().Add(-250*time.Millisecond))
	m.recordSnapshotCompressionPreemption(t.Context(), snapshotCompressionPreemptionRestoreInstance, target)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	assertMetricNames(t, rm, []string{
		"hypeman_snapshot_compression_jobs_total",
		"hypeman_snapshot_compression_duration_seconds",
		"hypeman_snapshot_compression_wait_duration_seconds",
		"hypeman_snapshot_compression_saved_bytes",
		"hypeman_snapshot_compression_ratio",
		"hypeman_snapshot_codec_fallbacks_total",
		"hypeman_snapshot_restore_memory_prepare_total",
		"hypeman_snapshot_restore_memory_prepare_duration_seconds",
		"hypeman_snapshot_compression_preemptions_total",
		"hypeman_snapshot_compression_active_total",
		"hypeman_snapshot_compression_pending_total",
	})

	jobsMetric := findMetric(t, rm, "hypeman_snapshot_compression_jobs_total")
	jobs, ok := jobsMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, jobs.DataPoints, 2)
	for _, point := range jobs.DataPoints {
		switch metricLabel(t, point.Attributes, "result") {
		case "success":
			assert.Equal(t, int64(1), point.Value)
			assert.Equal(t, "cloud-hypervisor", metricLabel(t, point.Attributes, "hypervisor"))
			assert.Equal(t, "lz4", metricLabel(t, point.Attributes, "algorithm"))
			assert.Equal(t, "standby", metricLabel(t, point.Attributes, "source"))
		case "skipped":
			assert.Equal(t, int64(1), point.Value)
			assert.Equal(t, "qemu", metricLabel(t, point.Attributes, "hypervisor"))
			assert.Equal(t, "zstd", metricLabel(t, point.Attributes, "algorithm"))
			assert.Equal(t, "standby", metricLabel(t, point.Attributes, "source"))
		default:
			t.Fatalf("unexpected compression job result datapoint: %s", metricLabel(t, point.Attributes, "result"))
		}
	}

	savedBytesMetric := findMetric(t, rm, "hypeman_snapshot_compression_saved_bytes")
	savedBytes, ok := savedBytesMetric.Data.(metricdata.Histogram[int64])
	require.True(t, ok)
	require.Len(t, savedBytes.DataPoints, 1)
	assert.Equal(t, uint64(1), savedBytes.DataPoints[0].Count)
	assert.Equal(t, int64(768), savedBytes.DataPoints[0].Sum)

	fallbackMetric := findMetric(t, rm, "hypeman_snapshot_codec_fallbacks_total")
	fallbacks, ok := fallbackMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, fallbacks.DataPoints, 1)
	assert.Equal(t, int64(1), fallbacks.DataPoints[0].Value)
	assert.Equal(t, "lz4", metricLabel(t, fallbacks.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "compress", metricLabel(t, fallbacks.DataPoints[0].Attributes, "operation"))
	assert.Equal(t, "missing_binary", metricLabel(t, fallbacks.DataPoints[0].Attributes, "reason"))

	restorePrepMetric := findMetric(t, rm, "hypeman_snapshot_restore_memory_prepare_total")
	restorePrep, ok := restorePrepMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, restorePrep.DataPoints, 1)
	assert.Equal(t, int64(1), restorePrep.DataPoints[0].Value)
	assert.Equal(t, "raw", metricLabel(t, restorePrep.DataPoints[0].Attributes, "restore_source"))
	assert.Equal(t, "success", metricLabel(t, restorePrep.DataPoints[0].Attributes, "result"))

	preemptionsMetric := findMetric(t, rm, "hypeman_snapshot_compression_preemptions_total")
	preemptions, ok := preemptionsMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, preemptions.DataPoints, 1)
	assert.Equal(t, int64(1), preemptions.DataPoints[0].Value)
	assert.Equal(t, "restore_instance", metricLabel(t, preemptions.DataPoints[0].Attributes, "operation"))

	activeMetric := findMetric(t, rm, "hypeman_snapshot_compression_active_total")
	active, ok := activeMetric.Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, active.DataPoints, 1)
	assert.Equal(t, int64(1), active.DataPoints[0].Value)
	assert.Equal(t, "lz4", metricLabel(t, active.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "standby", metricLabel(t, active.DataPoints[0].Attributes, "source"))

	pendingMetric := findMetric(t, rm, "hypeman_snapshot_compression_pending_total")
	pending, ok := pendingMetric.Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, pending.DataPoints, 1)
	assert.Equal(t, int64(1), pending.DataPoints[0].Value)
	assert.Equal(t, "zstd", metricLabel(t, pending.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "standby", metricLabel(t, pending.DataPoints[0].Attributes, "source"))

	waitMetric := findMetric(t, rm, "hypeman_snapshot_compression_wait_duration_seconds")
	waitDurations, ok := waitMetric.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, waitDurations.DataPoints, 1)
	assert.Equal(t, "skipped", metricLabel(t, waitDurations.DataPoints[0].Attributes, "outcome"))
}

func TestLifecycleEventMetrics_ObserveSubscribersQueueDepthAndDrops(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	m := &manager{
		paths:           paths.New(t.TempDir()),
		lifecycleEvents: newLifecycleSubscribers(),
	}

	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics
	m.lifecycleEvents.onDrop = func(ctx context.Context, consumer LifecycleEventConsumer) {
		m.recordLifecycleEventDropped(ctx, consumer, lifecycleEventDropReasonBufferFull)
	}

	waitCh, waitUnsub := m.SubscribeLifecycleEvents(LifecycleEventConsumerWaitForState)
	defer waitUnsub()
	autoCh, autoUnsub := m.SubscribeLifecycleEvents(LifecycleEventConsumerAutoStandby)
	defer autoUnsub()

	for i := 0; i < 3; i++ {
		m.lifecycleEvents.Notify(t.Context(), LifecycleEvent{
			Action:     LifecycleEventUpdate,
			InstanceID: "inst-1",
			Instance:   &Instance{State: StateRunning},
		})
	}
	<-waitCh

	for i := 0; i < m.lifecycleEvents.bufferSize; i++ {
		m.lifecycleEvents.Notify(t.Context(), LifecycleEvent{
			Action:     LifecycleEventUpdate,
			InstanceID: "inst-1",
			Instance:   &Instance{State: StateRunning},
		})
	}

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	assertMetricNames(t, rm, []string{
		"hypeman_instances_lifecycle_subscribers_total",
		"hypeman_instances_lifecycle_subscriber_queue_depth",
		"hypeman_instances_lifecycle_events_dropped_total",
	})

	subscribersMetric := findMetric(t, rm, "hypeman_instances_lifecycle_subscribers_total")
	subscribers, ok := subscribersMetric.Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, subscribers.DataPoints, len(allLifecycleEventConsumers))
	for _, point := range subscribers.DataPoints {
		switch metricLabel(t, point.Attributes, "consumer") {
		case string(LifecycleEventConsumerWaitForState):
			assert.Equal(t, int64(1), point.Value)
		case string(LifecycleEventConsumerAutoStandby):
			assert.Equal(t, int64(1), point.Value)
		case string(LifecycleEventConsumerHealthCheck):
			assert.Equal(t, int64(0), point.Value)
		case string(LifecycleEventConsumerRestartPolicy):
			assert.Equal(t, int64(0), point.Value)
		default:
			t.Fatalf("unexpected consumer label: %s", metricLabel(t, point.Attributes, "consumer"))
		}
	}

	queueDepthMetric := findMetric(t, rm, "hypeman_instances_lifecycle_subscriber_queue_depth")
	queueDepth, ok := queueDepthMetric.Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, queueDepth.DataPoints, len(allLifecycleEventConsumers))
	for _, point := range queueDepth.DataPoints {
		switch metricLabel(t, point.Attributes, "consumer") {
		case string(LifecycleEventConsumerWaitForState):
			assert.Equal(t, int64(m.lifecycleEvents.bufferSize), point.Value)
		case string(LifecycleEventConsumerAutoStandby):
			assert.Equal(t, int64(m.lifecycleEvents.bufferSize), point.Value)
		case string(LifecycleEventConsumerHealthCheck):
			assert.Equal(t, int64(0), point.Value)
		case string(LifecycleEventConsumerRestartPolicy):
			assert.Equal(t, int64(0), point.Value)
		default:
			t.Fatalf("unexpected consumer label: %s", metricLabel(t, point.Attributes, "consumer"))
		}
	}

	droppedMetric := findMetric(t, rm, "hypeman_instances_lifecycle_events_dropped_total")
	dropped, ok := droppedMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.NotEmpty(t, dropped.DataPoints)

	var waitDrops int64
	var autoDrops int64
	for _, point := range dropped.DataPoints {
		assert.Equal(t, string(lifecycleEventDropReasonBufferFull), metricLabel(t, point.Attributes, "reason"))
		switch metricLabel(t, point.Attributes, "consumer") {
		case string(LifecycleEventConsumerWaitForState):
			waitDrops += point.Value
		case string(LifecycleEventConsumerAutoStandby):
			autoDrops += point.Value
		}
	}
	assert.Greater(t, waitDrops, int64(0))
	assert.Greater(t, autoDrops, int64(0))
	assert.Equal(t, m.lifecycleEvents.bufferSize, len(autoCh))
}

func TestInstanceOldestInStateMetric_ObserveOldestAgePerState(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	now := time.Date(2026, time.March, 25, 12, 0, 0, 0, time.UTC)
	m := &manager{
		paths: paths.New(t.TempDir()),
		now:   func() time.Time { return now },
	}

	stoppedOldID := "stopped-old"
	require.NoError(t, m.ensureDirectories(stoppedOldID))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             stoppedOldID,
		Name:           stoppedOldID,
		CreatedAt:      now.Add(-2 * time.Hour),
		DataDir:        m.paths.InstanceDir(stoppedOldID),
		SocketPath:     m.paths.InstanceSocket(stoppedOldID, "cloud-hypervisor.sock"),
		HypervisorType: hypervisor.TypeCloudHypervisor,
	}}))

	stoppedNewID := "stopped-new"
	require.NoError(t, m.ensureDirectories(stoppedNewID))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             stoppedNewID,
		Name:           stoppedNewID,
		CreatedAt:      now.Add(-30 * time.Minute),
		DataDir:        m.paths.InstanceDir(stoppedNewID),
		SocketPath:     m.paths.InstanceSocket(stoppedNewID, "cloud-hypervisor.sock"),
		HypervisorType: hypervisor.TypeCloudHypervisor,
	}}))

	standbyOldID := "standby-old"
	require.NoError(t, m.ensureDirectories(standbyOldID))
	require.NoError(t, os.MkdirAll(m.paths.InstanceSnapshotLatest(standbyOldID), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(m.paths.InstanceSnapshotLatest(standbyOldID), "config.json"), []byte("{}"), 0644))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             standbyOldID,
		Name:           standbyOldID,
		CreatedAt:      now.Add(-90 * time.Minute),
		DataDir:        m.paths.InstanceDir(standbyOldID),
		SocketPath:     m.paths.InstanceSocket(standbyOldID, "qemu.sock"),
		HypervisorType: hypervisor.TypeQEMU,
	}}))

	standbyNewID := "standby-new"
	require.NoError(t, m.ensureDirectories(standbyNewID))
	require.NoError(t, os.MkdirAll(m.paths.InstanceSnapshotLatest(standbyNewID), 0755))
	require.NoError(t, os.WriteFile(filepath.Join(m.paths.InstanceSnapshotLatest(standbyNewID), "config.json"), []byte("{}"), 0644))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             standbyNewID,
		Name:           standbyNewID,
		CreatedAt:      now.Add(-45 * time.Minute),
		DataDir:        m.paths.InstanceDir(standbyNewID),
		SocketPath:     m.paths.InstanceSocket(standbyNewID, "qemu.sock"),
		HypervisorType: hypervisor.TypeQEMU,
	}}))

	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	assertMetricNames(t, rm, []string{
		"hypeman_instances_total",
		"hypeman_instances_oldest_in_state_seconds",
	})

	countsMetric := findMetric(t, rm, "hypeman_instances_total")
	counts, ok := countsMetric.Data.(metricdata.Gauge[int64])
	require.True(t, ok)
	require.Len(t, counts.DataPoints, 2)

	for _, point := range counts.DataPoints {
		state := metricLabel(t, point.Attributes, "state")
		hypervisorType := metricLabel(t, point.Attributes, "hypervisor")
		switch {
		case state == string(StateStopped) && hypervisorType == string(hypervisor.TypeCloudHypervisor):
			assert.Equal(t, int64(2), point.Value)
		case state == string(StateStandby) && hypervisorType == string(hypervisor.TypeQEMU):
			assert.Equal(t, int64(2), point.Value)
		default:
			t.Fatalf("unexpected count datapoint state=%s hypervisor=%s", state, hypervisorType)
		}
	}

	oldestMetric := findMetric(t, rm, "hypeman_instances_oldest_in_state_seconds")
	oldest, ok := oldestMetric.Data.(metricdata.Gauge[float64])
	require.True(t, ok)
	require.Len(t, oldest.DataPoints, 2)

	for _, point := range oldest.DataPoints {
		state := metricLabel(t, point.Attributes, "state")
		hypervisorType := metricLabel(t, point.Attributes, "hypervisor")
		switch {
		case state == string(StateStopped) && hypervisorType == string(hypervisor.TypeCloudHypervisor):
			assert.InDelta(t, (2 * time.Hour).Seconds(), point.Value, 0.001)
		case state == string(StateStandby) && hypervisorType == string(hypervisor.TypeQEMU):
			assert.InDelta(t, (90 * time.Minute).Seconds(), point.Value, 0.001)
		default:
			t.Fatalf("unexpected oldest-age datapoint state=%s hypervisor=%s", state, hypervisorType)
		}
	}
}

func TestInstanceTimeToRunningMetric_RecordWhenBootMarkersPersisted(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	tmpDir := t.TempDir()
	m := &manager{
		paths: paths.New(tmpDir),
	}

	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics

	id := "time-to-running-instance"
	require.NoError(t, m.ensureDirectories(id))

	bootStart := time.Date(2026, time.March, 25, 12, 0, 0, 0, time.UTC)
	programStartedAt := bootStart.Add(2 * time.Second)
	guestAgentReadyAt := bootStart.Add(3500 * time.Millisecond)

	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             id,
		Name:           id,
		DataDir:        m.paths.InstanceDir(id),
		SocketPath:     m.paths.InstanceSocket(id, "cloud-hypervisor.sock"),
		StartedAt:      &bootStart,
		HypervisorType: hypervisor.TypeCloudHypervisor,
	}}))

	logPath := m.paths.InstanceAppLog(id)
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o755))
	require.NoError(t, os.WriteFile(logPath, []byte(
		"HYPEMAN-PROGRAM-START ts="+programStartedAt.Format(time.RFC3339Nano)+" mode=exec\n"+
			"HYPEMAN-AGENT-READY ts="+guestAgentReadyAt.Format(time.RFC3339Nano)+"\n",
	), 0o644))
	require.NoError(t, os.Chtimes(logPath, bootStart.Add(time.Second), bootStart.Add(time.Second)))

	m.persistBootMarkers(t.Context(), id)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	assertMetricNames(t, rm, []string{
		"hypeman_instances_time_to_running_seconds",
	})

	timeToRunningMetric := findMetric(t, rm, "hypeman_instances_time_to_running_seconds")
	timeToRunning, ok := timeToRunningMetric.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, timeToRunning.DataPoints, 1)
	assert.Equal(t, uint64(1), timeToRunning.DataPoints[0].Count)
	assert.InDelta(t, 3.5, timeToRunning.DataPoints[0].Sum, 0.001)
	assert.Equal(t, "cloud-hypervisor", metricLabel(t, timeToRunning.DataPoints[0].Attributes, "hypervisor"))
}

func TestLifecycleDurationMetrics_RecordCompressionLabels(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	m := &manager{
		paths: paths.New(t.TempDir()),
	}
	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics

	level := 3
	m.recordDurationWithCompression(
		t.Context(),
		m.metrics.restoreDuration,
		time.Now().Add(-150*time.Millisecond),
		"success",
		hypervisor.TypeCloudHypervisor,
		&snapshotstore.SnapshotCompressionConfig{
			Enabled:   true,
			Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
			Level:     &level,
		},
	)
	m.recordDurationWithCompression(
		t.Context(),
		m.metrics.standbyDuration,
		time.Now().Add(-100*time.Millisecond),
		"success",
		hypervisor.TypeQEMU,
		nil,
	)

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	restoreMetric := findMetric(t, rm, "hypeman_instances_restore_duration_seconds")
	restore, ok := restoreMetric.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, restore.DataPoints, 1)
	assert.Equal(t, "success", metricLabel(t, restore.DataPoints[0].Attributes, "status"))
	assert.Equal(t, "cloud-hypervisor", metricLabel(t, restore.DataPoints[0].Attributes, "hypervisor"))
	assert.Equal(t, "zstd", metricLabel(t, restore.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "3", metricLabel(t, restore.DataPoints[0].Attributes, "level"))

	standbyMetric := findMetric(t, rm, "hypeman_instances_standby_duration_seconds")
	standby, ok := standbyMetric.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, standby.DataPoints, 1)
	assert.Equal(t, "success", metricLabel(t, standby.DataPoints[0].Attributes, "status"))
	assert.Equal(t, "qemu", metricLabel(t, standby.DataPoints[0].Attributes, "hypervisor"))
	assert.Equal(t, "none", metricLabel(t, standby.DataPoints[0].Attributes, "algorithm"))
	assert.Equal(t, "none", metricLabel(t, standby.DataPoints[0].Attributes, "level"))
}

func TestEnsureSnapshotMemoryReadySkipsPendingCompressionWithoutPreemptionMetric(t *testing.T) {
	t.Parallel()

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	mgr, _ := setupTestManager(t)
	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, mgr)
	require.NoError(t, err)
	mgr.metrics = metrics

	delay := 30 * time.Second
	timer := newFakeCompressionTimer()
	mgr.compressionTimerFactory = func(got time.Duration) compressionTimer {
		require.Equal(t, delay, got)
		return timer
	}

	snapshotDir := t.TempDir()
	rawPath := filepath.Join(snapshotDir, "memory")
	require.NoError(t, os.WriteFile(rawPath, []byte("pending raw snapshot"), 0o644))

	target := compressionTarget{
		Key:            "instance:pending",
		OwnerID:        "pending",
		SnapshotDir:    snapshotDir,
		HypervisorType: hypervisor.TypeCloudHypervisor,
		Source:         snapshotCompressionSourceStandby,
		Policy: snapshotstore.SnapshotCompressionConfig{
			Enabled:   true,
			Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
			Level:     intPtr(1),
		},
		Delay: delay,
	}

	mgr.startCompressionJob(t.Context(), target)

	require.Eventually(t, func() bool {
		mgr.compressionMu.Lock()
		defer mgr.compressionMu.Unlock()
		job, ok := mgr.compressionJobs[target.Key]
		return ok && job.state == compressionJobStatePendingDelay
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, mgr.ensureSnapshotMemoryReady(t.Context(), snapshotDir, target.Key, hypervisor.TypeCloudHypervisor))

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	jobsMetric := findMetric(t, rm, "hypeman_snapshot_compression_jobs_total")
	jobs, ok := jobsMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, jobs.DataPoints, 1)
	assert.Equal(t, "skipped", metricLabel(t, jobs.DataPoints[0].Attributes, "result"))

	waitMetric := findMetric(t, rm, "hypeman_snapshot_compression_wait_duration_seconds")
	waitDurations, ok := waitMetric.Data.(metricdata.Histogram[float64])
	require.True(t, ok)
	require.Len(t, waitDurations.DataPoints, 1)
	assert.Equal(t, "skipped", metricLabel(t, waitDurations.DataPoints[0].Attributes, "outcome"))

	assert.False(t, metricExists(rm, "hypeman_snapshot_compression_preemptions_total"), "pending-delay cancellation should not record a preemption")
}

func TestVGPUReconcileFailureMetric_RecordStages(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	reader := otelmetric.NewManualReader()
	provider := otelmetric.NewMeterProvider(otelmetric.WithReader(reader))

	m := &manager{
		paths: paths.New(t.TempDir()),
		reconcileVGPUDevices: func(context.Context, map[string]struct{}) error {
			return errors.New("sweep failed")
		},
	}
	metrics, err := newInstanceMetrics(provider.Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics

	const id = "unreadable"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: id}}))
	instanceDir := filepath.Dir(m.paths.InstanceMetadata(id))
	require.NoError(t, os.Chmod(instanceDir, 0o000))
	t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })

	m.ReconcileVGPUs(t.Context())

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(t.Context(), &rm))

	failuresMetric := findMetric(t, rm, "hypeman_instances_vgpu_reconcile_failures_total")
	failures, ok := failuresMetric.Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, failures.DataPoints, 1)
	assert.Equal(t, "list_instances", metricLabel(t, failures.DataPoints[0].Attributes, "stage"))
	assert.Equal(t, int64(1), failures.DataPoints[0].Value)
}

func assertMetricNames(t *testing.T, rm metricdata.ResourceMetrics, expected []string) {
	t.Helper()

	metricNames := make(map[string]bool)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			metricNames[m.Name] = true
		}
	}

	for _, name := range expected {
		assert.True(t, metricNames[name], "expected metric %s to be registered", name)
	}
}

func metricExists(rm metricdata.ResourceMetrics, name string) bool {
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return true
			}
		}
	}
	return false
}

func findMetric(t *testing.T, rm metricdata.ResourceMetrics, name string) metricdata.Metrics {
	t.Helper()

	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name == name {
				return m
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return metricdata.Metrics{}
}

func metricLabel(t *testing.T, attrs attribute.Set, key string) string {
	t.Helper()

	value, ok := attrs.Value(attribute.Key(key))
	require.True(t, ok, "expected label %s", key)
	return value.AsString()
}
