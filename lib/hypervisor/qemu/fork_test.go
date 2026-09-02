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
	if standardMachineType() != MachineTypeQ35 {
		t.Skip("UEFI snapshot configuration requires q35")
	}
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
		BootMode:      hypervisor.BootModeUEFI,
		Firmware: &hypervisor.FirmwareConfig{
			CodePath:   sourceDir + "/OVMF_CODE.fd",
			VarsPath:   sourceDir + "/OVMF_VARS.fd",
			SecureBoot: true,
		},
		TPM: &hypervisor.TPMConfig{
			SocketPath: sourceDir + "/swtpm.sock",
			StateDir:   sourceDir + "/tpm",
		},
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
	require.NoError(t, saveVMConfig(snapshotDir, savedVMConfig{VMConfig: initial, MachineType: MachineTypeQ35, QEMUVersion: "8.2.0"}))

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

	assert.Equal(t, MachineTypeQ35, updated.MachineType)
	assert.Equal(t, "8.2.0", updated.QEMUVersion)
	assert.Equal(t, int64(54321), updated.VsockCID)
	assert.Equal(t, targetDir+"/vsock/fork-vsock.sock", updated.VsockSocket)
	assert.Equal(t, targetDir+"/logs/fork-app.log", updated.SerialLogPath)
	require.NotNil(t, updated.Firmware)
	assert.Equal(t, targetDir+"/OVMF_CODE.fd", updated.Firmware.CodePath)
	assert.Equal(t, targetDir+"/OVMF_VARS.fd", updated.Firmware.VarsPath)
	require.NotNil(t, updated.TPM)
	assert.Equal(t, targetDir+"/swtpm.sock", updated.TPM.SocketPath)
	assert.Equal(t, targetDir+"/tpm", updated.TPM.StateDir)
	assert.Equal(t, targetDir+"/overlay.raw", updated.Disks[0].Path)
	assert.Equal(t, "/volumes/volume-data.raw", updated.Disks[1].Path, "non-instance paths should remain unchanged")
	require.Len(t, updated.Networks, 1)
	assert.Equal(t, "hype-newtap", updated.Networks[0].TAPDevice)
	assert.Equal(t, "10.100.20.20", updated.Networks[0].IP)
	assert.Equal(t, "02:00:00:dd:ee:ff", updated.Networks[0].MAC)
	assert.Equal(t, "255.255.0.0", updated.Networks[0].Netmask)
	require.NoError(t, starter.profile.validateConfig(updated.VMConfig))
}

func TestRewriteQEMUConfigPathsDirectBoot(t *testing.T) {
	sourceDir := "/src/guest"
	targetDir := "/dst/guest"
	updated := rewriteQEMUConfigPaths(hypervisor.VMConfig{
		KernelPath: sourceDir + "/kernel/vmlinuz",
		InitrdPath: sourceDir + "/kernel/initrd",
		KernelArgs: "console=ttyS0 root=" + sourceDir + "/rootfs",
	}, sourceDir, targetDir)

	assert.Equal(t, targetDir+"/kernel/vmlinuz", updated.KernelPath)
	assert.Equal(t, targetDir+"/kernel/initrd", updated.InitrdPath)
	assert.Equal(t, "console=ttyS0 root="+sourceDir+"/rootfs", updated.KernelArgs)
}
