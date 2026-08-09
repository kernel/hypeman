package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateVGPUHypervisor(t *testing.T) {
	t.Parallel()

	assert.NoError(t, validateVGPUHypervisor(hypervisor.TypeQEMU))
	assert.EqualError(t, validateVGPUHypervisor(hypervisor.TypeCloudHypervisor), "vGPU is only supported with qemu, got cloud-hypervisor")
}

func TestCleanupFailedCreateRetainsVGPUAssignment(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	stored := &StoredMetadata{
		Id:             "failed-create",
		Name:           "failed-create",
		GPUProfile:     "NVIDIA L40S-2Q",
		GPUFramework:   devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath:  "/sys/bus/pci/devices/0000:82:00.4",
		GPUMdevUUID:    "mdev-uuid",
		NetworkEnabled: true,
		IP:             "192.0.2.1",
		Volumes:        []VolumeAttachment{{VolumeID: "volume"}},
		HypervisorType: "qemu",
		DataDir:        m.paths.InstanceDir("failed-create"),
	}

	assert.True(t, m.cleanupFailedCreate(context.Background(), stored.Id, stored))

	retained, err := m.loadMetadata(stored.Id)
	require.NoError(t, err)
	assert.Equal(t, stored.Id, retained.Id)
	assert.Equal(t, stored.GPUFramework, retained.GPUFramework)
	assert.Equal(t, stored.GPUDevicePath, retained.GPUDevicePath)
	assert.Equal(t, stored.GPUMdevUUID, retained.GPUMdevUUID)
	assert.Empty(t, retained.Name)
	assert.Empty(t, retained.GPUProfile)
	assert.False(t, retained.NetworkEnabled)
	assert.Empty(t, retained.IP)
	assert.Empty(t, retained.Volumes)
	assert.Empty(t, retained.DataDir)
}

func TestCleanupFailedCreateDeletesDataWithoutRetainedVGPU(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("failed-create"))

	assert.False(t, m.cleanupFailedCreate(context.Background(), "failed-create", nil))
	_, err := m.loadMetadata("failed-create")
	require.Error(t, err)
}

func TestCleanupFailedCreateReportsUnpersistedRetention(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	const id = "failed-create"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, os.Mkdir(m.paths.InstanceMetadata(id), 0o755))

	stored := &StoredMetadata{
		Id:            id,
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	assert.False(t, m.cleanupFailedCreate(context.Background(), stored.Id, stored))
	_, err := m.loadMetadata(id)
	require.Error(t, err)
}

func TestCleanupFailedCreateReportsRetainedWhenFullMetadataSurvives(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	m := &manager{paths: paths.New(t.TempDir())}
	const id = "failed-create"
	require.NoError(t, m.ensureDirectories(id))
	stored := &StoredMetadata{
		Id:            id,
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: *stored}))

	instanceDir := filepath.Dir(m.paths.InstanceMetadata(id))
	require.NoError(t, os.Chmod(instanceDir, 0o555))
	t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })

	assert.True(t, m.cleanupFailedCreate(context.Background(), id, stored))
	retained, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, stored.GPUFramework, retained.GPUFramework)
	assert.Equal(t, stored.GPUDevicePath, retained.GPUDevicePath)
}

func TestVGPUCleanupPendingErrorUnwraps(t *testing.T) {
	t.Parallel()

	cause := errors.New("boot failed")
	retained := &VGPUCleanupPendingError{InstanceID: "inst-1", Retained: true, Err: cause}
	assert.ErrorIs(t, retained, cause)
	assert.Equal(t, "boot failed; vGPU release failed during rollback, instance inst-1 retains the assignment", retained.Error())

	unpersisted := &VGPUCleanupPendingError{InstanceID: "inst-1", Err: cause}
	assert.ErrorIs(t, unpersisted, cause)
	assert.Equal(t, "boot failed; vGPU release failed during rollback and the retention record for instance inst-1 could not be saved; the assignment is recovered on the next startup reconcile", unpersisted.Error())
}

func newStartRollbackVGPUManager(t *testing.T, destroy func(context.Context, devices.VGPUAssignment) error) (*manager, string) {
	t.Helper()
	m := &manager{
		paths:           paths.New(t.TempDir()),
		imageManager:    readyFixtureImageManager{name: "test-image"},
		instanceLocks:   sync.Map{},
		bootMarkerScans: sync.Map{},
		createVGPU: func(_ context.Context, profileName, _ string) (*devices.VGPUDevice, error) {
			return &devices.VGPUDevice{
				Framework:   devices.VGPUFrameworkVendorVFIO,
				VFAddress:   "0000:82:00.4",
				ProfileType: "1148",
				ProfileName: profileName,
				SysfsPath:   "/sys/bus/pci/devices/0000:82:00.4",
			}, nil
		},
		destroyVGPU: destroy,
	}
	const id = "start-rollback"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:             id,
		Name:           id,
		Image:          "test-image",
		GPUProfile:     "NVIDIA L40S-2Q",
		HypervisorType: lifecycleNoopHypervisorType,
		SocketPath:     m.paths.InstanceSocket(id, "noop.sock"),
		DataDir:        m.paths.InstanceDir(id),
	}}))
	return m, id
}

func TestStartRollbackClearsVGPUAssignmentAfterSuccessfulDestroy(t *testing.T) {
	var destroyed []devices.VGPUAssignment
	m, id := newStartRollbackVGPUManager(t, func(_ context.Context, assignment devices.VGPUAssignment) error {
		destroyed = append(destroyed, assignment)
		return nil
	})

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	_, err := m.startInstance(context.Background(), id, StartInstanceRequest{})
	require.Error(t, err)

	require.Len(t, destroyed, 1)
	assert.Equal(t, devices.VGPUAssignment{
		Framework:  devices.VGPUFrameworkVendorVFIO,
		DevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		InstanceID: id,
	}, destroyed[0])

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "NVIDIA L40S-2Q", stored.GPUProfile)
	assert.Empty(t, stored.GPUFramework)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Empty(t, stored.GPUMdevUUID)
}

func TestStartRollbackRetainsVGPUAssignmentAfterFailedDestroy(t *testing.T) {
	m, id := newStartRollbackVGPUManager(t, func(context.Context, devices.VGPUAssignment) error {
		return errors.New("destroy failed")
	})

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	_, err := m.startInstance(context.Background(), id, StartInstanceRequest{})
	require.Error(t, err)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, devices.VGPUFrameworkVendorVFIO, stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
}

func TestVGPUAssignmentClaimedByLiveInstanceFailsOnInvalidMetadata(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("invalid-instance"))
	require.NoError(t, os.WriteFile(m.paths.InstanceMetadata("invalid-instance"), []byte("{"), 0o644))

	_, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.Error(t, err)
}

func TestVGPUAssignmentClaimedByLiveInstanceNormalizesLegacyMdevPath(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("legacy-claimant"))
	pid := os.Getpid()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "legacy-claimant",
		Name:          "legacy-claimant",
		GPUMdevUUID:   "legacy-uuid",
		HypervisorPID: &pid,
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/mdev/devices/legacy-uuid")
	require.NoError(t, err)
	assert.True(t, claimed)
}

func TestVGPUAssignmentClaimedByLiveInstanceProtectsNilPIDClaim(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("booting-claimant"))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "booting-claimant",
		Name:          "booting-claimant",
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.NoError(t, err)
	assert.True(t, claimed, "a matching claim without a persisted PID must be treated as live: the PID is only persisted after the claimant boots")
}

func TestVGPUAssignmentClaimedByLiveInstanceIgnoresDeadClaim(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("dead-claimant"))
	deadPID := 1 << 30
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "dead-claimant",
		Name:          "dead-claimant",
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		HypervisorPID: &deadPID,
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.NoError(t, err)
	assert.False(t, claimed, "a claim whose hypervisor is gone must not block the release")
}

func TestReleaseStoredVGPUSkipsClaimScanForMdev(t *testing.T) {
	t.Parallel()

	m := &manager{
		paths:       paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error { return nil },
	}
	require.NoError(t, m.ensureDirectories("invalid-instance"))
	require.NoError(t, os.WriteFile(m.paths.InstanceMetadata("invalid-instance"), []byte("{"), 0o644))

	stored := &StoredMetadata{
		Id:            "mdev-instance",
		GPUFramework:  devices.VGPUFrameworkMdev,
		GPUMdevUUID:   "uuid-1",
		GPUDevicePath: "/sys/bus/mdev/devices/uuid-1",
	}
	require.NoError(t, m.releaseStoredVGPU(context.Background(), stored),
		"an unreadable metadata file must not block mdev releases")
	assert.Empty(t, stored.GPUDevicePath)
}

func TestStoredVGPUDevicePath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", storedVGPUDevicePath(&StoredMetadata{
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		GPUMdevUUID:   "legacy-uuid",
	}))
	assert.Equal(t, "/sys/bus/mdev/devices/legacy-uuid", storedVGPUDevicePath(&StoredMetadata{
		GPUMdevUUID: "legacy-uuid",
	}))
	assert.Empty(t, storedVGPUDevicePath(&StoredMetadata{}))
}

func TestReleaseStoredVGPURetainsMetadataOnFailure(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	stored := &StoredMetadata{
		GPUFramework:  devices.VGPUFramework("future-framework"),
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	err := m.releaseStoredVGPU(context.Background(), stored)
	assert.Error(t, err)
	assert.Equal(t, devices.VGPUFramework("future-framework"), stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
}

func TestSetAndClearStoredVGPUDevice(t *testing.T) {
	t.Parallel()

	stored := &StoredMetadata{}
	setStoredVGPUDevice(stored, &devices.VGPUDevice{
		Framework: devices.VGPUFrameworkVendorVFIO,
		SysfsPath: "/sys/bus/pci/devices/0000:82:00.4",
	})
	assert.Equal(t, devices.VGPUFrameworkVendorVFIO, stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)

	clearStoredVGPUDevice(stored)
	assert.Empty(t, stored.GPUFramework)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Empty(t, stored.GPUMdevUUID)
}
