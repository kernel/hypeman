package providers

import (
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/instances"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBalloonVMForInstance(t *testing.T) {
	t.Parallel()
	const gib = int64(1024 * 1024 * 1024)

	// No ceiling: assigned and baseline are both size + hotplug.
	noCeiling := balloonVMForInstance(instances.Instance{StoredMetadata: instances.StoredMetadata{
		Id: "a", Size: 2 * gib, HotplugSize: gib,
	}})
	assert.Equal(t, 3*gib, noCeiling.AssignedMemoryBytes)
	assert.Equal(t, 3*gib, noCeiling.BaselineMemoryBytes)

	// Ceiling above size: assigned is the ceiling, baseline is the shim's Size.
	withCeiling := balloonVMForInstance(instances.Instance{StoredMetadata: instances.StoredMetadata{
		Id: "b", Size: gib, MemoryCeilingBytes: 4 * gib,
	}})
	assert.Equal(t, 4*gib, withCeiling.AssignedMemoryBytes)
	assert.Equal(t, gib, withCeiling.BaselineMemoryBytes)

	// A ceiling not above size leaves the VM ordinary (baseline == assigned).
	lowCeiling := balloonVMForInstance(instances.Instance{StoredMetadata: instances.StoredMetadata{
		Id: "c", Size: 2 * gib, MemoryCeilingBytes: gib,
	}})
	assert.Equal(t, 2*gib, lowCeiling.AssignedMemoryBytes)
	assert.Equal(t, 2*gib, lowCeiling.BaselineMemoryBytes)

	// A vz ceiling VM with a (vz-ignored) hotplug size still baselines at Size:
	// the shim balloons to MemoryBytes (== Size), not Size + HotplugSize.
	ceilingWithHotplug := balloonVMForInstance(instances.Instance{StoredMetadata: instances.StoredMetadata{
		Id: "d", Size: gib, HotplugSize: gib, MemoryCeilingBytes: 4 * gib,
	}})
	assert.Equal(t, 4*gib, ceilingWithHotplug.AssignedMemoryBytes)
	assert.Equal(t, gib, ceilingWithHotplug.BaselineMemoryBytes)
}

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

func intPtr(v int) *int {
	return &v
}
