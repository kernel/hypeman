package system

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDefaultKernelVersion(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(paths.New(tmpDir))

	kernelVer := mgr.GetDefaultKernelVersion()
	assert.Equal(t, DefaultKernelVersion, kernelVer)
}

func TestGetKernelPath(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(paths.New(tmpDir))

	// Get kernel path
	kernelPath, err := mgr.GetKernelPath(DefaultKernelVersion)
	require.NoError(t, err)
	assert.Contains(t, kernelPath, "kernel")
	assert.Contains(t, kernelPath, "vmlinux")
}

func TestEnsureSystemFiles(t *testing.T) {
	// This test requires network access and takes a while
	// Skip by default, run explicitly with: go test -run TestEnsureSystemFiles
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	ctx := context.Background()
	mgr := NewManager(paths.New(tmpDir)).(*manager)

	// Ensure files
	err := mgr.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Verify kernel exists
	kernelPath, err := mgr.GetKernelPath(DefaultKernelVersion)
	require.NoError(t, err)
	assert.FileExists(t, kernelPath)

	// Verify initrd exists
	initrdPath, err := mgr.GetInitrdPath()
	require.NoError(t, err)
	assert.FileExists(t, initrdPath)

	// Verify idempotency - second call should succeed quickly
	err = mgr.EnsureSystemFiles(ctx)
	require.NoError(t, err)
}

func TestInitBinaryEmbedded(t *testing.T) {
	// Verify the init binary is embedded and has reasonable size
	// The Go init binary should be at least 1MB when statically linked
	assert.NotEmpty(t, InitBinary, "init binary should be embedded")
	assert.Greater(t, len(InitBinary), 100000, "init binary should be at least 100KB")
}

func TestInitrdEnsureLockKeyResolvesSymlinks(t *testing.T) {
	target := filepath.Join(t.TempDir(), "shared-initrd")
	require.NoError(t, os.MkdirAll(target, 0755))

	wrapperRoot := t.TempDir()
	wrapper := filepath.Join(wrapperRoot, "initrd")
	require.NoError(t, os.Symlink(target, wrapper))

	assert.Equal(t, initrdEnsureLockKey(target), initrdEnsureLockKey(wrapper))
}
