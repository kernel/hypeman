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

func TestSnapshotDefaultsFromConfigPreservesLevelForLZ4(t *testing.T) {
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

func TestParseInstanceLimitsParsesAggregateNamePatternLimits(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Limits: config.LimitsConfig{
			MaxVcpusPerInstance:  4,
			MaxMemoryPerInstance: "8GB",
			MaxOverlaySize:       "20GB",
			NamePatterns: []config.NamePatternLimitsConfig{
				{
					Pattern:                  "^team-a-",
					MaxTotalVcpus:            intPtr(32),
					MaxTotalMemory:           strPtr("256GB"),
					MaxTotalDisk:             strPtr("1TB"),
					MaxTotalNetworkBandwidth: strPtr("10Gbps"),
					MaxTotalDiskIO:           strPtr("1GB/s"),
				},
			},
		},
	}

	limits, err := parseInstanceLimits(cfg)
	require.NoError(t, err)
	require.Len(t, limits.NamePatterns, 1)

	pattern := limits.NamePatterns[0]
	require.NotNil(t, pattern.MaxTotalVcpus)
	assert.Equal(t, 32, *pattern.MaxTotalVcpus)
	require.NotNil(t, pattern.MaxTotalMemory)
	assert.Equal(t, int64(256*1024*1024*1024), *pattern.MaxTotalMemory)
	require.NotNil(t, pattern.MaxTotalDisk)
	assert.Equal(t, int64(1024*1024*1024*1024), *pattern.MaxTotalDisk)
	require.NotNil(t, pattern.MaxTotalNetworkBandwidth)
	assert.Equal(t, int64(10*1000*1000*1000/8), *pattern.MaxTotalNetworkBandwidth)
	require.NotNil(t, pattern.MaxTotalDiskIO)
	assert.Equal(t, int64(1024*1024*1024), *pattern.MaxTotalDiskIO)
}

func TestParseInstanceLimitsPreservesPerInstanceFields(t *testing.T) {
	t.Parallel()

	cfg := &config.Config{
		Limits: config.LimitsConfig{
			MaxVcpusPerInstance:  4,
			MaxMemoryPerInstance: "8GB",
			MaxOverlaySize:       "20GB",
			NamePatterns: []config.NamePatternLimitsConfig{
				{
					Pattern:              "^small-",
					MaxVcpusPerInstance:  intPtr(2),
					MaxMemoryPerInstance: strPtr("4GB"),
					MaxOverlaySize:       strPtr("10GB"),
				},
			},
		},
	}

	limits, err := parseInstanceLimits(cfg)
	require.NoError(t, err)

	resolved := limits.ForName("small-worker")
	assert.Equal(t, 2, resolved.MaxVcpusPerInstance)
	assert.Equal(t, int64(4*1024*1024*1024), resolved.MaxMemoryPerInstance)
	assert.Equal(t, int64(10*1024*1024*1024), resolved.MaxOverlaySize)
}

func intPtr(v int) *int {
	return &v
}

func strPtr(v string) *string {
	return &v
}
