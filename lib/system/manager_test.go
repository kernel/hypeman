package system

import (
	"context"
	"testing"

	"github.com/onkernel/hypeman/lib/paths"
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

func TestInitScriptGeneration(t *testing.T) {
	script := GenerateInitScript()

	// Verify script contains essential components
	assert.Contains(t, script, "#!/bin/sh")
	assert.Contains(t, script, "mount -t overlay")
	assert.Contains(t, script, "/dev/vda") // rootfs disk
	assert.Contains(t, script, "/dev/vdb") // overlay disk
	assert.Contains(t, script, "/dev/vdc") // config disk
	assert.Contains(t, script, "guest-agent")  // vsock guest agent
	assert.Contains(t, script, "${ENTRYPOINT}")
	assert.Contains(t, script, "wait $APP_PID") // Supervisor pattern
}
