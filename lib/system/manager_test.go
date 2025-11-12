package system

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetDefaultVersions(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(tmpDir)

	kernelVer, initrdVer := mgr.GetDefaultVersions()
	assert.Equal(t, DefaultKernelVersion, kernelVer)
	assert.Equal(t, DefaultInitrdVersion, initrdVer)
}

func TestGetPaths(t *testing.T) {
	tmpDir := t.TempDir()
	mgr := NewManager(tmpDir)

	// Get kernel path
	kernelPath, err := mgr.GetKernelPath(KernelCH_6_12_8_20250613)
	require.NoError(t, err)
	assert.Contains(t, kernelPath, "kernel/ch-release-v6.12.8-20250613")
	assert.Contains(t, kernelPath, "vmlinux")

	// Get initrd path
	initrdPath, err := mgr.GetInitrdPath(InitrdV1_0_0)
	require.NoError(t, err)
	assert.Contains(t, initrdPath, "initrd/v1.0.0")
	assert.Contains(t, initrdPath, "initrd")
}

func TestEnsureSystemFiles(t *testing.T) {
	// This test requires network access and takes a while
	// Skip by default, run explicitly with: go test -run TestEnsureSystemFiles
	if testing.Short() {
		t.Skip("Skipping integration test in short mode")
	}

	tmpDir := t.TempDir()
	ctx := context.Background()
	mgr := NewManager(tmpDir).(*manager)

	// Ensure files
	err := mgr.EnsureSystemFiles(ctx)
	require.NoError(t, err)

	// Verify kernel exists
	kernelPath, err := mgr.GetKernelPath(DefaultKernelVersion)
	require.NoError(t, err)
	assert.FileExists(t, kernelPath)

	// Verify initrd exists
	initrdPath, err := mgr.GetInitrdPath(DefaultInitrdVersion)
	require.NoError(t, err)
	assert.FileExists(t, initrdPath)

	// Verify idempotency - second call should succeed quickly
	err = mgr.EnsureSystemFiles(ctx)
	require.NoError(t, err)
}

func TestInitScriptGeneration(t *testing.T) {
	script := GenerateInitScript(InitrdV1_0_0)

	// Verify script contains essential components
	assert.Contains(t, script, "#!/bin/sh")
	assert.Contains(t, script, "mount -t overlay")
	assert.Contains(t, script, "/dev/vda") // rootfs disk
	assert.Contains(t, script, "/dev/vdb") // overlay disk
	assert.Contains(t, script, "/dev/vdc") // config disk
	assert.Contains(t, script, "exec chroot")
	assert.Contains(t, script, "${ENTRYPOINT}")
	assert.Contains(t, script, "v1.0.0") // Version in script
}

