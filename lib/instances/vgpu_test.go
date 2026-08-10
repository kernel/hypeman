package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/network"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupFailedCreateRetainsVGPUAssignment(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	assignedAt := time.Now().UTC()
	stored := &StoredMetadata{
		Id:             "failed-create",
		Name:           "failed-create",
		GPUProfile:     "NVIDIA L40S-2Q",
		GPUFramework:   devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath:  "/sys/bus/pci/devices/0000:82:00.4",
		GPUMdevUUID:    "mdev-uuid",
		GPUAssignedAt:  &assignedAt,
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
	assert.Equal(t, stored.GPUAssignedAt, retained.GPUAssignedAt)
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

func TestVGPUDevicePendingCleanup(t *testing.T) {
	t.Parallel()

	device := devices.VGPUDevice{
		Framework: devices.VGPUFrameworkVendorVFIO,
		SysfsPath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	cause := errors.New("rollback failed")
	pending := &devices.VGPUCreateCleanupPendingError{Device: device, Err: cause}

	wrapped := fmt.Errorf("create failed: %w", pending)
	actual, ok := vgpuDevicePendingCleanup(wrapped)
	require.True(t, ok)
	assert.Equal(t, device, *actual)

	assignedAt := time.Now().UTC()
	retained := retainedVGPUFromCreateError("inst-1", assignedAt, wrapped)
	require.NotNil(t, retained)
	assert.Equal(t, "inst-1", retained.Id)
	assert.Equal(t, device.Framework, retained.GPUFramework)
	assert.Equal(t, device.SysfsPath, retained.GPUDevicePath)
	assert.Equal(t, assignedAt, *retained.GPUAssignedAt)

	actual, ok = vgpuDevicePendingCleanup(cause)
	assert.False(t, ok)
	assert.Nil(t, actual)
	assert.Nil(t, retainedVGPUFromCreateError("inst-1", assignedAt, cause))
}

type startRetentionNetworkManager struct {
	network.Manager
	config       network.NetworkConfig
	releaseCalls int
}

func (m *startRetentionNetworkManager) CreateAllocation(context.Context, network.AllocateRequest) (*network.NetworkConfig, error) {
	config := m.config
	return &config, nil
}

func (m *startRetentionNetworkManager) ReleaseAllocation(context.Context, *network.Allocation) error {
	m.releaseCalls++
	return nil
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
		HypervisorType: hypervisor.TypeQEMU,
		SocketPath:     m.paths.InstanceSocket(id, "noop.sock"),
		DataDir:        m.paths.InstanceDir(id),
	}}))
	return m, id
}

func TestStartRetainsVGPUWhenCreateRollbackFails(t *testing.T) {
	m, id := newStartRollbackVGPUManager(t, func(context.Context, devices.VGPUAssignment) error {
		return nil
	})
	networkManager := &startRetentionNetworkManager{config: network.NetworkConfig{
		IP:        "192.0.2.20",
		MAC:       "02:00:00:00:00:20",
		TAPDevice: "tap-new",
	}}
	m.networkManager = networkManager

	previousProgramStart := time.Now().Add(-time.Hour).UTC()
	previousExitCode := 23
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.NetworkEnabled = true
	meta.IP = "192.0.2.10"
	meta.MAC = "02:00:00:00:00:10"
	meta.Entrypoint = []string{"old-entrypoint"}
	meta.Cmd = []string{"old-command"}
	meta.ProgramStartedAt = &previousProgramStart
	meta.ExitCode = &previousExitCode
	meta.ExitMessage = "previous exit"
	require.NoError(t, m.saveMetadata(meta))

	device := devices.VGPUDevice{
		Framework:   devices.VGPUFrameworkVendorVFIO,
		VFAddress:   "0000:82:00.4",
		ProfileType: "1148",
		ProfileName: "NVIDIA L40S-2Q",
		SysfsPath:   "/sys/bus/pci/devices/0000:82:00.4",
	}
	cause := errors.New("create verification and rollback failed")
	m.createVGPU = func(context.Context, string, string) (*devices.VGPUDevice, error) {
		return nil, &devices.VGPUCreateCleanupPendingError{Device: device, Err: cause}
	}

	_, err = m.startInstance(context.Background(), id, StartInstanceRequest{
		Entrypoint: []string{"new-entrypoint"},
		Cmd:        []string{"new-command"},
	})
	require.ErrorIs(t, err, cause)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, device.Framework, stored.GPUFramework)
	assert.Equal(t, device.SysfsPath, stored.GPUDevicePath)
	assert.NotNil(t, stored.GPUAssignedAt)
	assert.Equal(t, []string{"old-entrypoint"}, stored.Entrypoint)
	assert.Equal(t, []string{"old-command"}, stored.Cmd)
	assert.Equal(t, previousProgramStart, *stored.ProgramStartedAt)
	assert.Equal(t, previousExitCode, *stored.ExitCode)
	assert.Equal(t, "previous exit", stored.ExitMessage)
	assert.Equal(t, "192.0.2.10", stored.IP)
	assert.Equal(t, "02:00:00:00:00:10", stored.MAC)
	assert.Equal(t, 1, networkManager.releaseCalls)
}

func TestStartDoesNotRestrictVGPUHypervisor(t *testing.T) {
	m, id := newStartRollbackVGPUManager(t, func(context.Context, devices.VGPUAssignment) error {
		return nil
	})
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.HypervisorType = hypervisor.TypeCloudHypervisor
	require.NoError(t, m.saveMetadata(meta))

	cause := errors.New("create failed")
	m.createVGPU = func(context.Context, string, string) (*devices.VGPUDevice, error) {
		return nil, cause
	}

	_, err = m.startInstance(context.Background(), id, StartInstanceRequest{})
	assert.ErrorIs(t, err, cause)
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
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	stalePID := os.Getpid()
	meta.HypervisorPID = &stalePID
	meta.HypervisorStartTime = 1
	meta.HypervisorBootID = "previous-boot"
	require.NoError(t, m.saveMetadata(meta))

	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "missing"))
	_, err = m.startInstance(context.Background(), id, StartInstanceRequest{Entrypoint: []string{"new-entrypoint"}})
	require.Error(t, err)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, devices.VGPUFrameworkVendorVFIO, stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
	assert.NotNil(t, stored.GPUAssignedAt)
	assert.Nil(t, stored.HypervisorPID)
	assert.Zero(t, stored.HypervisorStartTime)
	assert.Empty(t, stored.HypervisorBootID)
	assert.Empty(t, stored.Entrypoint)
}

func TestCleanupStartVGPURestoresMetadataAfterBootFailure(t *testing.T) {
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			return nil
		},
	}
	const id = "failed-start"
	require.NoError(t, m.ensureDirectories(id))

	previousStart := time.Now().Add(-time.Hour).UTC()
	previousProgramStart := previousStart.Add(time.Second)
	exitCode := 1
	rollbackMeta := metadata{StoredMetadata: StoredMetadata{
		Id:               id,
		GPUProfile:       "NVIDIA L40S-2Q",
		Entrypoint:       []string{"old-entrypoint"},
		Cmd:              []string{"old-command"},
		StartedAt:        &previousStart,
		ProgramStartedAt: &previousProgramStart,
		ExitCode:         &exitCode,
		ExitMessage:      "previous exit",
	}}

	partial := rollbackMeta
	partial.Entrypoint = []string{"new-entrypoint"}
	partial.Cmd = []string{"new-command"}
	partial.StartedAt = ptr(time.Now().UTC())
	partial.ProgramStartedAt = nil
	partial.ExitCode = nil
	partial.ExitMessage = ""
	assignedAt := time.Now().UTC()
	device := &devices.VGPUDevice{
		Framework: devices.VGPUFrameworkVendorVFIO,
		SysfsPath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	setStoredVGPUDevice(&partial.StoredMetadata, device, assignedAt)
	require.NoError(t, m.saveMetadata(&partial))

	m.cleanupStartVGPU(context.Background(), id, device, assignedAt, rollbackMeta)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, rollbackMeta.Entrypoint, stored.Entrypoint)
	assert.Equal(t, rollbackMeta.Cmd, stored.Cmd)
	assert.Equal(t, rollbackMeta.StartedAt, stored.StartedAt)
	assert.Equal(t, rollbackMeta.ProgramStartedAt, stored.ProgramStartedAt)
	assert.Equal(t, rollbackMeta.ExitCode, stored.ExitCode)
	assert.Equal(t, rollbackMeta.ExitMessage, stored.ExitMessage)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Nil(t, stored.GPUAssignedAt)
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

func TestVGPUAssignmentClaimedByLiveInstanceErrorsOnRecentNilPIDClaim(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("booting-claimant"))
	assignedAt := time.Now().UTC()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "booting-claimant",
		Name:          "booting-claimant",
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		GPUAssignedAt: &assignedAt,
	}}))

	_, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "booting-claimant")
}

func TestVGPUAssignmentClaimedByLiveInstanceIgnoresStaleNilPIDClaim(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("stale-claimant"))
	assignedAt := time.Now().Add(-VGPUAssignmentStartupGracePeriod - time.Minute)
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "stale-claimant",
		Name:          "stale-claimant",
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
		GPUAssignedAt: &assignedAt,
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance(context.Background(), "other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.NoError(t, err)
	assert.False(t, claimed)
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

func TestReleaseStoredVGPURetainsRequesterOnAmbiguousClaim(t *testing.T) {
	t.Parallel()

	const devicePath = "/sys/bus/pci/devices/0000:82:00.4"
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			t.Fatal("destroyVGPU must not be called for an ambiguous claim")
			return nil
		},
	}
	require.NoError(t, m.ensureDirectories("ambiguous-claimant"))
	assignedAt := time.Now().UTC()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            "ambiguous-claimant",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: devicePath,
		GPUAssignedAt: &assignedAt,
	}}))

	stored := &StoredMetadata{
		Id:            "requester",
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: devicePath,
	}
	err := m.releaseStoredVGPU(context.Background(), stored)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ambiguous-claimant")
	assert.Equal(t, devices.VGPUFrameworkVendorVFIO, stored.GPUFramework)
	assert.Equal(t, devicePath, stored.GPUDevicePath)
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

	assignedAt := time.Now().UTC()
	stored := &StoredMetadata{}
	setStoredVGPUDevice(stored, &devices.VGPUDevice{
		Framework: devices.VGPUFrameworkVendorVFIO,
		SysfsPath: "/sys/bus/pci/devices/0000:82:00.4",
	}, assignedAt)
	assert.Equal(t, devices.VGPUFrameworkVendorVFIO, stored.GPUFramework)
	assert.Equal(t, "/sys/bus/pci/devices/0000:82:00.4", stored.GPUDevicePath)
	assert.Equal(t, assignedAt, *stored.GPUAssignedAt)

	clearStoredVGPUDevice(stored)
	assert.Empty(t, stored.GPUFramework)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Empty(t, stored.GPUMdevUUID)
	assert.Nil(t, stored.GPUAssignedAt)
}
