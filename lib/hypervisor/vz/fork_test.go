//go:build darwin

package vz

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/vz/shimconfig"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPrepareFork_NoSnapshotPathIsNoOp(t *testing.T) {
	starter := NewStarter()
	result, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{})
	require.NoError(t, err)
	assert.False(t, result.VsockCIDUpdated)
}

func TestPrepareFork_RewritesSnapshotManifest(t *testing.T) {
	starter := NewStarter()
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, shimconfig.SnapshotManifestFile)

	sourceDir := "/src/guests/a"
	targetDir := "/dst/guests/b"
	orig := shimconfig.SnapshotManifest{
		Hypervisor:       string(hypervisor.TypeVZ),
		MachineStateFile: shimconfig.SnapshotMachineStateFile,
		ShimConfig: shimconfig.ShimConfig{
			SerialLogPath: sourceDir + "/logs/serial.log",
			KernelPath:    sourceDir + "/kernel/vmlinuz",
			InitrdPath:    sourceDir + "/kernel/initrd",
			ControlSocket: sourceDir + "/vz.sock",
			VsockSocket:   sourceDir + "/vz.vsock",
			LogPath:       sourceDir + "/logs/vz-shim.log",
			Disks: []shimconfig.DiskConfig{
				{Path: sourceDir + "/overlay.raw"},
				{Path: "/volumes/shared.raw"},
			},
			Networks: []shimconfig.NetworkConfig{
				{MAC: "02:00:00:00:00:01"},
			},
		},
	}
	data, err := json.Marshal(orig)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(manifestPath, data, 0644))

	result, err := starter.PrepareFork(context.Background(), hypervisor.ForkPrepareRequest{
		SnapshotConfigPath: manifestPath,
		SourceDataDir:      sourceDir,
		TargetDataDir:      targetDir,
		VsockCID:           22222,
		VsockSocket:        targetDir + "/fork.vsock",
		SerialLogPath:      targetDir + "/logs/fork-serial.log",
	})
	require.NoError(t, err)
	assert.False(t, result.VsockCIDUpdated)

	updatedData, err := os.ReadFile(manifestPath)
	require.NoError(t, err)

	var updated shimconfig.SnapshotManifest
	require.NoError(t, json.Unmarshal(updatedData, &updated))

	assert.Equal(t, string(hypervisor.TypeVZ), updated.Hypervisor)
	assert.Equal(t, shimconfig.SnapshotMachineStateFile, updated.MachineStateFile)
	assert.Equal(t, targetDir+"/logs/fork-serial.log", updated.ShimConfig.SerialLogPath)
	assert.Equal(t, targetDir+"/fork.vsock", updated.ShimConfig.VsockSocket)
	assert.Equal(t, targetDir+"/kernel/vmlinuz", updated.ShimConfig.KernelPath)
	assert.Equal(t, targetDir+"/kernel/initrd", updated.ShimConfig.InitrdPath)
	assert.Equal(t, targetDir+"/vz.sock", updated.ShimConfig.ControlSocket)
	assert.Equal(t, targetDir+"/logs/vz-shim.log", updated.ShimConfig.LogPath)
	require.Len(t, updated.ShimConfig.Disks, 2)
	assert.Equal(t, targetDir+"/overlay.raw", updated.ShimConfig.Disks[0].Path)
	assert.Equal(t, "/volumes/shared.raw", updated.ShimConfig.Disks[1].Path)
	require.Len(t, updated.ShimConfig.Networks, 1)
	assert.Equal(t, "02:00:00:00:00:01", updated.ShimConfig.Networks[0].MAC)
}
