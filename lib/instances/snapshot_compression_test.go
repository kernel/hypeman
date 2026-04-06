package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNormalizeCompressionConfig(t *testing.T) {
	t.Parallel()

	cfg, err := normalizeCompressionConfig(nil)
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)

	cfg, err = normalizeCompressionConfig(&snapshotstore.SnapshotCompressionConfig{
		Enabled: true,
	})
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmZstd, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 1, *cfg.Level)

	_, err = normalizeCompressionConfig(&snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(25),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))

	_, err = normalizeCompressionConfig(&snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
		Level:     intPtr(10),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))

	cfg, err = normalizeCompressionConfig(&snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
		Level:     intPtr(9),
	})
	require.NoError(t, err)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmLz4, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 9, *cfg.Level)

	cfg, err = normalizeCompressionConfig(&snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithm("ZSTD"),
		Level:     intPtr(3),
	})
	require.NoError(t, err)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmZstd, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 3, *cfg.Level)
}

func TestResolveSnapshotCompressionPolicyPrecedence(t *testing.T) {
	t.Parallel()

	m := &manager{
		snapshotDefaults: SnapshotPolicy{
			Compression: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     intPtr(2),
			},
		},
	}

	stored := &StoredMetadata{
		SnapshotPolicy: &SnapshotPolicy{
			Compression: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
			},
		},
	}

	cfg, err := m.resolveSnapshotCompressionPolicy(stored, &snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(4),
	})
	require.NoError(t, err)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmZstd, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 4, *cfg.Level)

	cfg, err = m.resolveSnapshotCompressionPolicy(stored, nil)
	require.NoError(t, err)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmLz4, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 0, *cfg.Level)

	cfg, err = m.resolveSnapshotCompressionPolicy(&StoredMetadata{}, nil)
	require.NoError(t, err)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmZstd, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 2, *cfg.Level)
}

func TestResolveStandbyCompressionPolicyIsOptInOnly(t *testing.T) {
	t.Parallel()

	m := &manager{}

	cfg, err := m.resolveStandbyCompressionPolicy(&StoredMetadata{
		HypervisorType: "cloud-hypervisor",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, cfg)

	cfg, err = m.resolveStandbyCompressionPolicy(&StoredMetadata{
		HypervisorType: "cloud-hypervisor",
	}, &snapshotstore.SnapshotCompressionConfig{Enabled: false})
	require.NoError(t, err)
	assert.Nil(t, cfg)

	cfg, err = m.resolveStandbyCompressionPolicy(&StoredMetadata{
		HypervisorType: "qemu",
	}, nil)
	require.NoError(t, err)
	assert.Nil(t, cfg)
}

func TestResolveCompressionPolicyExplicitDisableOverridesDefaults(t *testing.T) {
	t.Parallel()

	m := &manager{
		snapshotDefaults: SnapshotPolicy{
			Compression: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     intPtr(3),
			},
		},
	}

	stored := &StoredMetadata{
		SnapshotPolicy: &SnapshotPolicy{
			Compression: &snapshotstore.SnapshotCompressionConfig{
				Enabled: false,
			},
		},
	}

	cfg, err := m.resolveSnapshotCompressionPolicy(stored, nil)
	require.NoError(t, err)
	assert.False(t, cfg.Enabled)

	standbyCfg, err := m.resolveStandbyCompressionPolicy(stored, nil)
	require.NoError(t, err)
	assert.Nil(t, standbyCfg)
}

func TestResolveStandbyCompressionPolicyInvalidConfiguredDefaultIsInvalidRequest(t *testing.T) {
	t.Parallel()

	m := &manager{
		snapshotDefaults: SnapshotPolicy{
			Compression: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithm("brotli"),
			},
		},
	}

	_, err := m.resolveStandbyCompressionPolicy(&StoredMetadata{}, nil)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestResolveStandbyCompressionDelayPrecedence(t *testing.T) {
	t.Parallel()

	instanceDelay := 2 * time.Minute
	overrideDelay := 15 * time.Second
	m := &manager{}

	delay, err := m.resolveStandbyCompressionDelay(&StoredMetadata{
		SnapshotPolicy: &SnapshotPolicy{
			StandbyCompressionDelay: &instanceDelay,
		},
	}, &overrideDelay)
	require.NoError(t, err)
	assert.Equal(t, overrideDelay, delay)

	delay, err = m.resolveStandbyCompressionDelay(&StoredMetadata{
		SnapshotPolicy: &SnapshotPolicy{
			StandbyCompressionDelay: &instanceDelay,
		},
	}, nil)
	require.NoError(t, err)
	assert.Equal(t, instanceDelay, delay)

	delay, err = m.resolveStandbyCompressionDelay(&StoredMetadata{}, nil)
	require.NoError(t, err)
	assert.Zero(t, delay)
}

func TestResolveStandbyCompressionDelayRejectsNegativeDuration(t *testing.T) {
	t.Parallel()

	m := &manager{}
	negative := -1 * time.Second

	_, err := m.resolveStandbyCompressionDelay(&StoredMetadata{}, &negative)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestCompressionMetadataForExistingArtifactUsesActualAlgorithm(t *testing.T) {
	t.Parallel()

	cfg := compressionMetadataForExistingArtifact(snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(7),
	}, snapshotstore.SnapshotCompressionAlgorithmLz4)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmLz4, cfg.Algorithm)
	assert.Nil(t, cfg.Level)

	cfg = compressionMetadataForExistingArtifact(snapshotstore.SnapshotCompressionConfig{
		Enabled:   true,
		Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
		Level:     intPtr(7),
	}, snapshotstore.SnapshotCompressionAlgorithmZstd)
	assert.True(t, cfg.Enabled)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmZstd, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 7, *cfg.Level)
}

func TestValidateCreateRequestSnapshotPolicy(t *testing.T) {
	t.Parallel()

	req := &CreateInstanceRequest{
		Name:  "compression-test",
		Image: "docker.io/library/alpine:latest",
		SnapshotPolicy: &SnapshotPolicy{
			Compression: &snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     intPtr(0),
			},
		},
	}
	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestValidateCreateRequestRejectsNegativeStandbyCompressionDelay(t *testing.T) {
	t.Parallel()

	negative := -1 * time.Second
	req := &CreateInstanceRequest{
		Name:  "compression-delay-test",
		Image: "docker.io/library/alpine:latest",
		SnapshotPolicy: &SnapshotPolicy{
			StandbyCompressionDelay: &negative,
		},
	}

	err := validateCreateRequest(req)
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestValidateCreateSnapshotRequestRejectsStoppedCompression(t *testing.T) {
	t.Parallel()

	err := validateCreateSnapshotRequest(CreateSnapshotRequest{
		Kind: SnapshotKindStopped,
		Compression: &snapshotstore.SnapshotCompressionConfig{
			Enabled: true,
		},
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
}

func TestDeleteInstanceCancelsCompressionJob(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()
	const instanceID = "delete-instance-compression"

	require.NoError(t, mgr.ensureDirectories(instanceID))
	now := time.Now()
	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             instanceID,
		Name:           instanceID,
		DataDir:        mgr.paths.InstanceDir(instanceID),
		HypervisorType: hypervisor.TypeCloudHypervisor,
		CreatedAt:      now,
		StoppedAt:      &now,
	}}))

	target := installCancelableCompressionJob(mgr, compressionTarget{
		Key:         mgr.snapshotJobKeyForInstance(instanceID),
		OwnerID:     instanceID,
		SnapshotDir: mgr.paths.InstanceSnapshotLatest(instanceID),
		Source:      snapshotCompressionSourceStandby,
		Policy: snapshotstore.SnapshotCompressionConfig{
			Enabled:   true,
			Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
			Level:     intPtr(1),
		},
	})

	require.NoError(t, mgr.DeleteInstance(ctx, instanceID))
	assertCompressionJobCanceled(t, mgr, target)
	_, err := os.Stat(mgr.paths.InstanceDir(instanceID))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
}

func TestDeleteSnapshotCancelsCompressionJob(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	ctx := context.Background()
	const snapshotID = "delete-snapshot-compression"

	snapshotDir := mgr.paths.SnapshotGuestDir(snapshotID)
	require.NoError(t, os.MkdirAll(snapshotDir, 0o755))
	require.NoError(t, mgr.saveSnapshotRecord(&snapshotRecord{
		Snapshot: Snapshot{
			Id:        snapshotID,
			Name:      snapshotID,
			Kind:      SnapshotKindStandby,
			CreatedAt: time.Now(),
		},
	}))

	target := installCancelableCompressionJob(mgr, compressionTarget{
		Key:         mgr.snapshotJobKeyForSnapshot(snapshotID),
		SnapshotID:  snapshotID,
		SnapshotDir: snapshotDir,
		Source:      snapshotCompressionSourceSnapshot,
		Policy: snapshotstore.SnapshotCompressionConfig{
			Enabled:   true,
			Algorithm: snapshotstore.SnapshotCompressionAlgorithmLz4,
			Level:     intPtr(0),
		},
	})

	require.NoError(t, mgr.DeleteSnapshot(ctx, snapshotID))
	assertCompressionJobCanceled(t, mgr, target)
	_, err := os.Stat(mgr.paths.SnapshotDir(snapshotID))
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
}

func installCancelableCompressionJob(mgr *manager, target compressionTarget) *compressionTarget {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	mgr.compressionMu.Lock()
	mgr.compressionJobs[target.Key] = &compressionJob{
		cancel: cancel,
		done:   done,
		state:  compressionJobStateRunning,
		target: target,
	}
	mgr.compressionMu.Unlock()

	go func() {
		<-ctx.Done()
		mgr.compressionMu.Lock()
		delete(mgr.compressionJobs, target.Key)
		mgr.compressionMu.Unlock()
		close(done)
	}()

	return &target
}

func assertCompressionJobCanceled(t *testing.T, mgr *manager, target *compressionTarget) {
	t.Helper()

	require.Eventually(t, func() bool {
		mgr.compressionMu.Lock()
		defer mgr.compressionMu.Unlock()
		_, ok := mgr.compressionJobs[target.Key]
		return !ok
	}, time.Second, 10*time.Millisecond)
}

func TestStartCompressionJobDelayedCancellationRecordsSkipped(t *testing.T) {
	t.Parallel()

	mgr, _ := setupTestManager(t)
	delay := 45 * time.Second
	timer := newFakeCompressionTimer()
	mgr.compressionTimerFactory = func(got time.Duration) compressionTimer {
		require.Equal(t, delay, got)
		return timer
	}

	snapshotDir := t.TempDir()
	rawPath := filepath.Join(snapshotDir, "memory")
	require.NoError(t, os.WriteFile(rawPath, []byte("delayed standby snapshot"), 0o644))

	target := compressionTarget{
		Key:         "instance:delayed",
		OwnerID:     "delayed",
		SnapshotDir: snapshotDir,
		Source:      snapshotCompressionSourceStandby,
		Policy: snapshotstore.SnapshotCompressionConfig{
			Enabled:   true,
			Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
			Level:     intPtr(1),
		},
		Delay: delay,
	}

	mgr.startCompressionJob(context.Background(), target)

	require.Eventually(t, func() bool {
		mgr.compressionMu.Lock()
		defer mgr.compressionMu.Unlock()
		job, ok := mgr.compressionJobs[target.Key]
		return ok && job.state == compressionJobStatePendingDelay
	}, time.Second, 10*time.Millisecond)

	canceled, err := mgr.cancelAndWaitCompressionJob(context.Background(), target.Key)
	require.NoError(t, err)
	require.NotNil(t, canceled)
	assert.Equal(t, compressionJobStatePendingDelay, canceled.State)

	_, err = os.Stat(rawPath)
	require.NoError(t, err, "raw snapshot should remain available when delay is skipped")
	_, err = os.Stat(rawPath + ".zst")
	require.Error(t, err)
	assert.True(t, os.IsNotExist(err))
}

func TestRecoverPendingStandbyCompressionJobsRequeuesDelayedJob(t *testing.T) {
	mgr, _ := setupTestManager(t)
	now := time.Date(2026, time.April, 6, 12, 0, 0, 0, time.UTC)
	mgr.now = func() time.Time { return now }

	const instanceID = "recover-delayed"
	delay := 30 * time.Second
	snapshotDir := mgr.paths.InstanceSnapshotLatest(instanceID)
	rawPath := filepath.Join(snapshotDir, "memory")

	require.NoError(t, mgr.ensureDirectories(instanceID))
	require.NoError(t, os.MkdirAll(snapshotDir, 0o755))
	require.NoError(t, os.WriteFile(rawPath, []byte("pending standby snapshot"), 0o644))
	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             instanceID,
		Name:           instanceID,
		DataDir:        mgr.paths.InstanceDir(instanceID),
		HypervisorType: hypervisor.TypeCloudHypervisor,
		CreatedAt:      now,
		StoppedAt:      &now,
		PendingStandbyCompression: &PendingStandbyCompression{
			Policy: snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     intPtr(1),
			},
			NotBefore: now.Add(delay),
		},
	}}))

	timer := newFakeCompressionTimer()
	delayCh := make(chan time.Duration, 1)
	mgr.compressionTimerFactory = func(got time.Duration) compressionTimer {
		delayCh <- got
		return timer
	}

	require.NoError(t, mgr.recoverPendingStandbyCompressionJobs(context.Background()))

	require.Eventually(t, func() bool {
		mgr.compressionMu.Lock()
		defer mgr.compressionMu.Unlock()
		job, ok := mgr.compressionJobs[mgr.snapshotJobKeyForInstance(instanceID)]
		return ok && job.state == compressionJobStatePendingDelay
	}, time.Second, 10*time.Millisecond)
	select {
	case gotDelay := <-delayCh:
		assert.Equal(t, delay, gotDelay)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for recovered compression delay")
	}

	canceled, err := mgr.cancelAndWaitCompressionJob(context.Background(), mgr.snapshotJobKeyForInstance(instanceID))
	require.NoError(t, err)
	require.NotNil(t, canceled)
	assert.Equal(t, compressionJobStatePendingDelay, canceled.State)

	meta, err := mgr.loadMetadata(instanceID)
	require.NoError(t, err)
	assert.Nil(t, meta.StoredMetadata.PendingStandbyCompression)
	_, err = os.Stat(rawPath)
	require.NoError(t, err)
}

func TestRecoverPendingStandbyCompressionJobsStartsImmediateCompression(t *testing.T) {
	mgr, _ := setupTestManager(t)
	now := time.Date(2026, time.April, 6, 12, 5, 0, 0, time.UTC)
	mgr.now = func() time.Time { return now }

	const instanceID = "recover-immediate"
	snapshotDir := mgr.paths.InstanceSnapshotLatest(instanceID)
	rawPath := filepath.Join(snapshotDir, "memory")

	require.NoError(t, mgr.ensureDirectories(instanceID))
	require.NoError(t, os.MkdirAll(snapshotDir, 0o755))
	require.NoError(t, os.WriteFile(rawPath, []byte("standby snapshot that should compress now"), 0o644))
	require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             instanceID,
		Name:           instanceID,
		DataDir:        mgr.paths.InstanceDir(instanceID),
		HypervisorType: hypervisor.TypeCloudHypervisor,
		CreatedAt:      now,
		StoppedAt:      &now,
		PendingStandbyCompression: &PendingStandbyCompression{
			Policy: snapshotstore.SnapshotCompressionConfig{
				Enabled:   true,
				Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
				Level:     intPtr(1),
			},
			NotBefore: now.Add(-time.Second),
		},
	}}))
	mgr.compressionTimerFactory = func(time.Duration) compressionTimer {
		t.Fatal("unexpected delay timer for immediate recovery")
		return newFakeCompressionTimer()
	}

	require.NoError(t, mgr.recoverPendingStandbyCompressionJobs(context.Background()))

	require.Eventually(t, func() bool {
		meta, err := mgr.loadMetadata(instanceID)
		if err != nil {
			return false
		}
		_, rawExistsErr := os.Stat(rawPath)
		_, _, compressed := findCompressedSnapshotMemoryFile(snapshotDir)
		return meta.StoredMetadata.PendingStandbyCompression == nil && os.IsNotExist(rawExistsErr) && compressed
	}, 5*time.Second, 20*time.Millisecond)
}

func TestRecoverPendingStandbyCompressionJobsClearsStalePlans(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, mgr *manager, instanceID string, now time.Time)
	}{
		{
			name: "stopped_instance_without_snapshot",
			prepare: func(t *testing.T, mgr *manager, instanceID string, now time.Time) {
				t.Helper()
				require.NoError(t, mgr.ensureDirectories(instanceID))
				require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
					Id:             instanceID,
					Name:           instanceID,
					DataDir:        mgr.paths.InstanceDir(instanceID),
					HypervisorType: hypervisor.TypeCloudHypervisor,
					CreatedAt:      now,
					StoppedAt:      &now,
					PendingStandbyCompression: &PendingStandbyCompression{
						Policy: snapshotstore.SnapshotCompressionConfig{
							Enabled:   true,
							Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
							Level:     intPtr(1),
						},
						NotBefore: now.Add(time.Minute),
					},
				}}))
			},
		},
		{
			name: "already_compressed_snapshot",
			prepare: func(t *testing.T, mgr *manager, instanceID string, now time.Time) {
				t.Helper()
				snapshotDir := mgr.paths.InstanceSnapshotLatest(instanceID)
				require.NoError(t, mgr.ensureDirectories(instanceID))
				require.NoError(t, os.MkdirAll(snapshotDir, 0o755))
				require.NoError(t, os.WriteFile(filepath.Join(snapshotDir, "memory.zst"), []byte("compressed"), 0o644))
				require.NoError(t, mgr.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
					Id:             instanceID,
					Name:           instanceID,
					DataDir:        mgr.paths.InstanceDir(instanceID),
					HypervisorType: hypervisor.TypeCloudHypervisor,
					CreatedAt:      now,
					StoppedAt:      &now,
					PendingStandbyCompression: &PendingStandbyCompression{
						Policy: snapshotstore.SnapshotCompressionConfig{
							Enabled:   true,
							Algorithm: snapshotstore.SnapshotCompressionAlgorithmZstd,
							Level:     intPtr(1),
						},
						NotBefore: now.Add(time.Minute),
					},
				}}))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr, _ := setupTestManager(t)
			now := time.Date(2026, time.April, 6, 12, 10, 0, 0, time.UTC)
			mgr.now = func() time.Time { return now }
			instanceID := "recover-stale-" + tt.name

			tt.prepare(t, mgr, instanceID, now)

			require.NoError(t, mgr.recoverPendingStandbyCompressionJobs(context.Background()))

			meta, err := mgr.loadMetadata(instanceID)
			require.NoError(t, err)
			assert.Nil(t, meta.StoredMetadata.PendingStandbyCompression)

			mgr.compressionMu.Lock()
			_, ok := mgr.compressionJobs[mgr.snapshotJobKeyForInstance(instanceID)]
			mgr.compressionMu.Unlock()
			assert.False(t, ok)
		})
	}
}

type fakeCompressionTimer struct {
	ch      chan time.Time
	mu      sync.Mutex
	stopped bool
	fired   bool
}

func newFakeCompressionTimer() *fakeCompressionTimer {
	return &fakeCompressionTimer{ch: make(chan time.Time, 1)}
}

func (t *fakeCompressionTimer) Chan() <-chan time.Time {
	return t.ch
}

func (t *fakeCompressionTimer) Stop() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped || t.fired {
		return false
	}
	t.stopped = true
	return true
}

func (t *fakeCompressionTimer) Fire() bool {
	t.mu.Lock()
	if t.stopped || t.fired {
		t.mu.Unlock()
		return false
	}
	t.fired = true
	ch := t.ch
	t.mu.Unlock()

	ch <- time.Now()
	return true
}
