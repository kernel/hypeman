package providers

import (
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSnapshotDefaultsFromConfigDisabledReturnsNilCompression(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Snapshot: config.SnapshotConfig{
			CompressionDefault: config.SnapshotCompressionDefaultConfig{
				Enabled:   false,
				Algorithm: "lz4",
				Level:     intPtr(7),
			},
		},
	}

	defaults := snapshotDefaultsFromConfig(cfg)
	assert.Nil(t, defaults.Compression)
}

func TestSnapshotDefaultsFromConfigOmitsLevelForLZ4(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Snapshot: config.SnapshotConfig{
			CompressionDefault: config.SnapshotCompressionDefaultConfig{
				Enabled:   true,
				Algorithm: "lz4",
				Level:     intPtr(7),
			},
		},
	}

	defaults := snapshotDefaultsFromConfig(cfg)
	require.NotNil(t, defaults.Compression)
	assert.True(t, defaults.Compression.Enabled)
	assert.Equal(t, snapshotstore.SnapshotCompressionAlgorithmLz4, defaults.Compression.Algorithm)
	require.NotNil(t, defaults.Compression.Level)
	assert.Equal(t, 7, *defaults.Compression.Level)
}

func TestSnapshotDefaultsFromConfigKeepsZstdLevel(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Snapshot: config.SnapshotConfig{
			CompressionDefault: config.SnapshotCompressionDefaultConfig{
				Enabled:   true,
				Algorithm: "zstd",
				Level:     intPtr(5),
			},
		},
	}

	defaults := snapshotDefaultsFromConfig(cfg)
	require.NotNil(t, defaults.Compression)
	require.NotNil(t, defaults.Compression.Level)
	assert.Equal(t, 5, *defaults.Compression.Level)
}

func intPtr(v int) *int {
	return &v
}
