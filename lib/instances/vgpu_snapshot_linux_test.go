//go:build linux

package instances

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/stretchr/testify/require"
)

func gpuSnapshotManager(t *testing.T) (*manager, *metadata) {
	t.Helper()
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}, {PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	m.vmStarters = map[hypervisor.Type]hypervisor.VMStarter{hypervisor.TypeQEMU: qemu.NewStarter()}
	m.vendorVFIOProfiles = func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
		out := make(map[string][]devices.VGPUProfileType)
		for _, vf := range vfs {
			out[vf.PCIAddress] = []devices.VGPUProfileType{{Name: "NVIDIA L40S-3Q", TypeName: "profile-3q", FramebufferMB: 3072}}
		}
		return out, nil
	}
	m.pickVFIndex = pickFirst
	meta := saveTestVGPUInstance(t, m, "child")
	meta.GPUProfile = "NVIDIA L40S-3Q"
	meta.HypervisorType = hypervisor.TypeQEMU
	meta.HypervisorVersion = qemu.VGPUSnapshotVersion
	meta.VsockCID = 71
	require.NoError(t, m.saveMetadata(meta))
	return m, meta
}

func writeGPUSnapshotConfig(t *testing.T, m *manager, id string) {
	t.Helper()
	dir := m.paths.InstanceSnapshotLatest(id)
	require.NoError(t, os.MkdirAll(dir, 0o755))
	data, err := json.Marshal(map[string]any{"VGPUSnapshot": true, "InstanceID": "source", "VGPUDevicePath": testVFDevicePath, "VsockCID": 70, "MachineType": "pc-q35-8.2", "QEMUVersion": qemu.VGPUSnapshotVersion})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "qemu-config.json"), data, 0o600))
}

func TestVGPUSnapshotRestoreClaimsANewVFAndRollsBack(t *testing.T) {
	m, meta := gpuSnapshotManager(t)
	source := saveTestVGPUInstance(t, m, "source")
	source.GPUFramework = devices.VGPUFrameworkVendorVFIO
	source.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(source))
	writeGPUSnapshotConfig(t, m, meta.Id)
	reset := ""
	m.destroyVGPU = func(_ context.Context, a devices.VGPUAssignment) error { reset = a.DevicePath; return nil }
	original := meta.StoredMetadata
	meta.IP = "10.0.0.25"
	cleanup, err := m.prepareVGPUSnapshotRestore(context.Background(), meta, original)
	require.NoError(t, err)
	require.Equal(t, "/sys/bus/pci/devices/0000:82:00.5", meta.GPUDevicePath)
	persisted, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	require.Equal(t, meta.GPUDevicePath, persisted.GPUDevicePath)
	data, err := os.ReadFile(filepath.Join(m.paths.InstanceSnapshotLatest(meta.Id), "qemu-config.json"))
	require.NoError(t, err)
	var cfg hypervisor.VMConfig
	require.NoError(t, json.Unmarshal(data, &cfg))
	require.Equal(t, meta.GPUDevicePath, cfg.VGPUDevicePath)
	require.Equal(t, meta.Id, cfg.InstanceID)
	require.Equal(t, meta.VsockCID, cfg.VsockCID)
	cleanup()
	require.Equal(t, meta.GPUDevicePath, reset)
	persisted, err = m.loadMetadata(meta.Id)
	require.NoError(t, err)
	require.Empty(t, persisted.GPUDevicePath)
	require.Empty(t, persisted.IP, "failed restore must not retain the released network allocation")
	sourceAfter, err := m.loadMetadata(source.Id)
	require.NoError(t, err)
	require.Equal(t, testVFDevicePath, sourceAfter.GPUDevicePath)
}

func TestVGPUSnapshotRestoreFailureReleasesClaim(t *testing.T) {
	m, meta := gpuSnapshotManager(t)
	reset := false
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error { reset = true; return nil }
	_, err := m.prepareVGPUSnapshotRestore(context.Background(), meta, meta.StoredMetadata)
	require.ErrorContains(t, err, "bind snapshot")
	require.True(t, reset)
	persisted, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	require.Empty(t, persisted.GPUDevicePath)
}

func TestVGPUSnapshotRollbackPreservesLiveClaim(t *testing.T) {
	m, meta := gpuSnapshotManager(t)
	writeGPUSnapshotConfig(t, m, meta.Id)
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error { t.Fatal("live VF must not be reset"); return nil }
	cleanup, err := m.prepareVGPUSnapshotRestore(context.Background(), meta, meta.StoredMetadata)
	require.NoError(t, err)
	meta.HypervisorProcessIdentity.Set(os.Getpid())
	require.NoError(t, m.saveMetadata(meta))
	cleanup()
	persisted, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	require.Equal(t, meta.GPUDevicePath, persisted.GPUDevicePath)
}

func TestVGPUSnapshotRequiresExplicitSupportedRuntime(t *testing.T) {
	_, meta := gpuSnapshotManager(t)
	require.Error(t, validateVGPUSnapshotCapture(&meta.StoredMetadata))
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	require.NoError(t, validateVGPUSnapshotCapture(&meta.StoredMetadata))
	meta.HypervisorVersion = "8.2.0"
	require.False(t, usesVGPUSnapshot(&meta.StoredMetadata))
	require.Error(t, validateVGPUSnapshotCapture(&meta.StoredMetadata))
	req := CreateInstanceRequest{GPU: &GPUConfig{Profile: "NVIDIA L40S-3Q"}}
	require.NoError(t, validateVGPUSnapshotRequest(req, hypervisor.TypeQEMU, qemu.VGPUSnapshotVersion))
	req.SkipGuestAgent = true
	require.ErrorIs(t, validateVGPUSnapshotRequest(req, hypervisor.TypeQEMU, qemu.VGPUSnapshotVersion), ErrInvalidRequest)
}
