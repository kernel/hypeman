package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHostLockPreventsConcurrentOwnership(t *testing.T) {
	dataDir := t.TempDir()
	first, err := acquireHostLock(dataDir)
	require.NoError(t, err)

	second, err := os.OpenFile(filepath.Join(dataDir, hostLockFilename), os.O_RDWR, 0o600)
	require.NoError(t, err)
	defer second.Close()

	err = syscall.Flock(int(second.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	require.Error(t, err)
	require.True(t, errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN))

	require.NoError(t, releaseHostLock(first))
	require.NoError(t, syscall.Flock(int(second.Fd()), syscall.LOCK_EX|syscall.LOCK_NB))
	require.NoError(t, syscall.Flock(int(second.Fd()), syscall.LOCK_UN))
}
