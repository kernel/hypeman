package qemu

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareFork_NoSnapshotPathIsNoOp(t *testing.T) {
	starter := NewStarter()
	result, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{})
	require.NoError(t, err)
	assert.False(t, result.VsockCIDUpdated)
}

func TestPrepareFork_RewritesSnapshotConfig(t *testing.T) {
	starter := NewStarter()
	snapshotDir := t.TempDir()

	sourceDir := "/src/guest"
	targetDir := "/dst/guest"
	initial := hypervisor.VMConfig{
		VCPUs:         2,
		MemoryBytes:   2 * 1024 * 1024 * 1024,
		SerialLogPath: sourceDir + "/logs/app.log",
		VsockCID:      12345,
		VsockSocket:   sourceDir + "/vsock/vsock.sock",
		KernelPath:    sourceDir + "/kernel/vmlinuz",
		InitrdPath:    sourceDir + "/kernel/initrd",
		KernelArgs:    "console=ttyS0 root=" + sourceDir + "/rootfs note=keep-" + sourceDir + "-as-substring",
		Disks: []hypervisor.DiskConfig{
			{Path: sourceDir + "/overlay.raw"},
			{Path: "/volumes/volume-data.raw"},
		},
		Networks: []hypervisor.NetworkConfig{
			{
				TAPDevice: "hype-oldtap",
				IP:        "10.100.10.10",
				MAC:       "02:00:00:aa:bb:cc",
				Netmask:   "255.255.0.0",
			},
		},
	}
	require.NoError(t, saveVMConfig(snapshotDir, initial))

	result, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: filepath.Join(snapshotDir, "config.json"),
		SourceDataDir:      sourceDir,
		TargetDataDir:      targetDir,
		VsockCID:           54321,
		VsockSocket:        targetDir + "/vsock/fork-vsock.sock",
		SerialLogPath:      targetDir + "/logs/fork-app.log",
		Network: &hypervisor.ForkNetworkConfig{
			TAPDevice: "hype-newtap",
			IP:        "10.100.20.20",
			MAC:       "02:00:00:dd:ee:ff",
			Netmask:   "255.255.0.0",
		},
	})
	require.NoError(t, err)
	assert.True(t, result.VsockCIDUpdated)

	updated, err := loadVMConfig(snapshotDir)
	require.NoError(t, err)

	assert.Equal(t, int64(54321), updated.VsockCID)
	assert.Equal(t, targetDir+"/vsock/fork-vsock.sock", updated.VsockSocket)
	assert.Equal(t, targetDir+"/logs/fork-app.log", updated.SerialLogPath)
	assert.Equal(t, targetDir+"/kernel/vmlinuz", updated.KernelPath)
	assert.Equal(t, targetDir+"/kernel/initrd", updated.InitrdPath)
	assert.Equal(t, initial.KernelArgs, updated.KernelArgs)
	assert.Equal(t, targetDir+"/overlay.raw", updated.Disks[0].Path)
	assert.Equal(t, "/volumes/volume-data.raw", updated.Disks[1].Path, "non-instance paths should remain unchanged")
	require.Len(t, updated.Networks, 1)
	assert.Equal(t, "hype-newtap", updated.Networks[0].TAPDevice)
	assert.Equal(t, "10.100.20.20", updated.Networks[0].IP)
	assert.Equal(t, "02:00:00:dd:ee:ff", updated.Networks[0].MAC)
	assert.Equal(t, "255.255.0.0", updated.Networks[0].Netmask)
}
