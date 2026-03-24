package instances

import (
	"context"
	"errors"
	"os"
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
