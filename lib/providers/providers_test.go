package providers

import (
	"bytes"
	"errors"
	"log/slog"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	snapshotstore "github.com/kernel/hypeman/lib/snapshot"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrackInitialization(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&output, nil))
	finish := trackInitialization(log, "test component")
	finish(nil)

	logs := output.String()
	assert.Contains(t, logs, `"msg":"application component initialization started"`)
	assert.Contains(t, logs, `"msg":"application component initialization completed"`)
	assert.Contains(t, logs, `"component":"test component"`)
	assert.Contains(t, logs, `"duration":`)
}

func TestTrackInitializationFailure(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&output, nil))
	finish := trackInitialization(log, "test component")
	finish(errors.New("failed"))

	logs := output.String()
	assert.Contains(t, logs, `"msg":"application component initialization failed"`)
	assert.Contains(t, logs, `"error":"failed"`)
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
