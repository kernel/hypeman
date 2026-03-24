package hypervisor

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBalloonTargetCachePersistsAcrossProcessRestarts(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	var cache BalloonTargetCache
	cache.Store(socketPath, 384)

	var restarted BalloonTargetCache
	value, ok := restarted.Load(socketPath)
	require.True(t, ok)
	assert.Equal(t, int64(384), value)
}

func TestBalloonTargetCacheDeleteClearsIndexedKeyAfterSocketRemoval(t *testing.T) {
	t.Parallel()

	socketPath := testSocketPath(t)
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)

	var cache BalloonTargetCache
	cache.Store(socketPath, 512)

	require.NoError(t, listener.Close())
	if err := os.Remove(socketPath); err != nil && !os.IsNotExist(err) {
		require.NoError(t, err)
	}

	cache.Delete(socketPath)
	_, ok := cache.Load(socketPath)
	assert.False(t, ok)
}

func testSocketPath(t *testing.T) string {
	t.Helper()

	file, err := os.CreateTemp("", "btc-*.sock")
	require.NoError(t, err)
	path := file.Name()
	require.NoError(t, file.Close())
	require.NoError(t, os.Remove(path))
	t.Cleanup(func() {
		_ = os.Remove(path)
		_ = os.Remove(balloonTargetStatePath(path))
	})
	return filepath.Clean(path)
}
