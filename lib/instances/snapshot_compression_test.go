package instances

import (
	"errors"
	"testing"

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
		Level:     intPtr(1),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrInvalidRequest))
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
	assert.Nil(t, cfg.Level)

	cfg, err = m.resolveSnapshotCompressionPolicy(&StoredMetadata{}, nil)
	require.NoError(t, err)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmZstd, cfg.Algorithm)
	require.NotNil(t, cfg.Level)
	assert.Equal(t, 2, *cfg.Level)
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
