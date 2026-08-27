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

func persistTestVGPURetention(m *manager, ctx context.Context, id string, stub *StoredMetadata) bool {
	retention := vgpuRetention{instanceID: id, stub: stub, retained: stub != nil}
	m.persistVGPURetention(ctx, &retention)
	return retention.persisted
}

func TestCleanupFailedCreateRetainsVGPUAssignment(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	const id = "failed-create"
	stub := StoredMetadata{
		Id:             id,
		Name:           id,
		NetworkEnabled: true,
		IP:             "192.0.2.1",
		Volumes:        []VolumeAttachment{{VolumeID: "volume"}},
		HypervisorType: "qemu",
		DataDir:        m.paths.InstanceDir(id),
	}
	device := devices.VGPUDevice{
		ProfileName: "NVIDIA L40S-2Q",
		Framework:   devices.VGPUFrameworkVendorVFIO,
		SysfsPath:   "/sys/bus/pci/devices/0000:82:00.4",
	}
	assignedAt := time.Now().UTC()
	retention := vgpuRetention{instanceID: id}
	retention.retainFromCreateError(stub, assignedAt, &devices.VGPUCreateCleanupPendingError{Device: device, Err: errors.New("rollback failed")})

	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, os.WriteFile(m.paths.InstanceOverlay(id), []byte("overlay"), 0o644))
	require.NoError(t, os.WriteFile(m.paths.InstanceConfigDisk(id), []byte("config"), 0o644))
	require.NoError(t, os.MkdirAll(m.paths.InstanceVolumeOverlaysDir(id), 0o755))
	require.NoError(t, os.WriteFile(m.paths.InstanceVolumeOverlay(id, "volume"), []byte("volume overlay"), 0o644))

	m.persistVGPURetention(context.Background(), &retention)
	assert.True(t, retention.persisted)
	assert.NoFileExists(t, m.paths.InstanceOverlay(id))
	assert.NoFileExists(t, m.paths.InstanceConfigDisk(id))
	assert.NoDirExists(t, m.paths.InstanceVolumeOverlaysDir(id))

	retained, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, id, retained.Id)
	assert.Equal(t, device.Framework, retained.GPUFramework)
	assert.Equal(t, device.SysfsPath, retained.GPUDevicePath)
	assert.Equal(t, assignedAt, *retained.GPUAssignedAt)
	assert.Equal(t, stub.Name, retained.Name)
	assert.Equal(t, device.ProfileName, retained.GPUProfile)
	assert.Equal(t, stub.HypervisorType, retained.HypervisorType)
	assert.Equal(t, stub.DataDir, retained.DataDir)
	assert.False(t, retained.NetworkEnabled)
	assert.Empty(t, retained.IP)
	assert.Empty(t, retained.Volumes)
	assert.True(t, retained.GPURetainedForCleanup)
}

func TestCleanupFailedCreateDeletesDataWithoutRetainedVGPU(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("failed-create"))

	assert.False(t, persistTestVGPURetention(m, context.Background(), "failed-create", nil))
	_, err := m.loadMetadata("failed-create")
	require.Error(t, err)
}

func TestCleanupFailedCreateReportsUnpersistedRetention(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	m := &manager{paths: paths.New(t.TempDir())}
	const id = "failed-create"
	require.NoError(t, os.MkdirAll(m.paths.GuestsDir(), 0o755))
	require.NoError(t, os.Chmod(m.paths.GuestsDir(), 0o555))
	t.Cleanup(func() { _ = os.Chmod(m.paths.GuestsDir(), 0o755) })

	stored := &StoredMetadata{
		Id:            id,
		GPUFramework:  devices.VGPUFrameworkVendorVFIO,
		GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4",
	}
	assert.False(t, persistTestVGPURetention(m, context.Background(), stored.Id, stored))
	_, err := m.loadMetadata(id)
	require.Error(t, err, "the lost retention leaves no metadata claim, so the periodic sweep releases the VF")
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
	var pending *VGPUCleanupPendingError
	require.ErrorAs(t, err, &pending)
	assert.Equal(t, id, pending.InstanceID)
	assert.True(t, pending.Retained)

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

func TestStartReportsUnretainedVGPUWhenRetentionSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	m, id := newStartRollbackVGPUManager(t, func(context.Context, devices.VGPUAssignment) error {
		return nil
	})
	device := devices.VGPUDevice{
		Framework:   devices.VGPUFrameworkVendorVFIO,
		VFAddress:   "0000:82:00.4",
		ProfileType: "1148",
		ProfileName: "NVIDIA L40S-2Q",
		SysfsPath:   "/sys/bus/pci/devices/0000:82:00.4",
	}
	cause := errors.New("create verification and rollback failed")
	m.createVGPU = func(context.Context, string, string) (*devices.VGPUDevice, error) {
		instanceDir := filepath.Dir(m.paths.InstanceMetadata(id))
		require.NoError(t, os.Chmod(instanceDir, 0o555))
		t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })
		return nil, &devices.VGPUCreateCleanupPendingError{Device: device, Err: cause}
	}

	_, err := m.startInstance(context.Background(), id, StartInstanceRequest{})
	require.ErrorIs(t, err, cause)
	var pending *VGPUCleanupPendingError
	require.ErrorAs(t, err, &pending)
	assert.Equal(t, id, pending.InstanceID)
	assert.False(t, pending.Retained)

	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath, "retention save failed, so no assignment should be recorded")
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
	var pending *VGPUCleanupPendingError
	require.ErrorAs(t, err, &pending, "a retained rollback assignment must surface as vgpu_cleanup_pending")
	assert.Equal(t, id, pending.InstanceID)
	assert.True(t, pending.Retained)

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

func TestCleanupStartVGPUReportsRetentionWhenRollbackSaveFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory permissions")
	}

	tests := []struct {
		name            string
		assignmentSaved bool
		wantPersisted   bool
	}{
		{name: "assignment save survived", assignmentSaved: true, wantPersisted: true},
		{name: "assignment never saved", assignmentSaved: false, wantPersisted: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, id := newStartRollbackVGPUManager(t, func(context.Context, devices.VGPUAssignment) error {
				return errors.New("destroy failed")
			})
			device := devices.VGPUDevice{
				Framework: devices.VGPUFrameworkVendorVFIO,
				SysfsPath: "/sys/bus/pci/devices/0000:82:00.4",
			}
			assignedAt := time.Now().UTC()

			meta, err := m.loadMetadata(id)
			require.NoError(t, err)
			rollbackMeta := *meta
			if tt.assignmentSaved {
				setStoredVGPUDevice(&meta.StoredMetadata, &device, assignedAt)
				require.NoError(t, m.saveMetadata(meta))
			}

			instanceDir := filepath.Dir(m.paths.InstanceMetadata(id))
			require.NoError(t, os.Chmod(instanceDir, 0o555))
			t.Cleanup(func() { _ = os.Chmod(instanceDir, 0o755) })

			retained, persisted := m.cleanupStartVGPU(context.Background(), id, &device, assignedAt, rollbackMeta)
			assert.True(t, retained)
			assert.Equal(t, tt.wantPersisted, persisted)
		})
	}
}

func TestCleanupStartVGPURestoresMetadataAfterBootFailure(t *testing.T) {
	m := &manager{
		paths:       paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error { return nil },
	}
	const id = "failed-start"
	require.NoError(t, m.ensureDirectories(id))

	previousStart := time.Now().Add(-time.Hour).UTC()
	exitCode := 1
	rollbackMeta := metadata{StoredMetadata: StoredMetadata{
		Id:          id,
		Name:        "original name",
		GPUProfile:  "NVIDIA L40S-2Q",
		Entrypoint:  []string{"old-entrypoint"},
		StartedAt:   &previousStart,
		ExitCode:    &exitCode,
		ExitMessage: "previous exit",
	}}
	partial := metadata{StoredMetadata: StoredMetadata{Id: id, Name: "partial start"}}
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
	assert.Equal(t, rollbackMeta.StoredMetadata, stored.StoredMetadata)
}

func TestVGPUAssignmentClaimedByLiveInstanceFailsOnInvalidMetadata(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("invalid-instance"))
	require.NoError(t, os.WriteFile(m.paths.InstanceMetadata("invalid-instance"), []byte("{"), 0o644))

	_, err := m.vgpuAssignmentClaimedByLiveInstance("other-instance", "/sys/bus/pci/devices/0000:82:00.4")
	require.Error(t, err)
}

func TestVGPUAssignmentClaimedByLiveInstanceNormalizesLegacyMdevPath(t *testing.T) {
	t.Parallel()

	m := &manager{paths: paths.New(t.TempDir())}
	require.NoError(t, m.ensureDirectories("legacy-claimant"))
	pid := os.Getpid()
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:          "legacy-claimant",
		Name:        "legacy-claimant",
		GPUMdevUUID: "legacy-uuid",
		HypervisorProcessIdentity: HypervisorProcessIdentity{
			HypervisorPID:       &pid,
			HypervisorStartTime: processStartTime(pid),
			HypervisorBootID:    hostBootID(),
		},
	}}))

	claimed, err := m.vgpuAssignmentClaimedByLiveInstance("other-instance", "/sys/bus/mdev/devices/legacy-uuid")
	require.NoError(t, err)
	assert.True(t, claimed)
}

func TestVGPUAssignmentClaimedByLiveInstanceLiveness(t *testing.T) {
	t.Parallel()

	const devicePath = "/sys/bus/pci/devices/0000:82:00.4"
	deadPID := 1 << 30
	require.False(t, ProcessExists(deadPID))
	recent := time.Now().UTC()
	stale := recent.Add(-devices.VGPUAssignmentGracePeriod - time.Minute)
	tests := []struct {
		name       string
		assignedAt *time.Time
		pid        *int
		wantErr    string
	}{
		{name: "recent without PID", assignedAt: &recent, wantErr: "no persisted hypervisor PID"},
		{name: "stale without PID", assignedAt: &stale},
		{name: "recent dead PID", assignedAt: &recent, pid: &deadPID, wantErr: "recorded hypervisor is not running"},
		{name: "legacy dead PID", pid: &deadPID},
	}
	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			m := &manager{paths: paths.New(t.TempDir())}
			id := fmt.Sprintf("claimant-%d", i)
			require.NoError(t, m.ensureDirectories(id))
			require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
				Id:                        id,
				GPUFramework:              devices.VGPUFrameworkVendorVFIO,
				GPUDevicePath:             devicePath,
				GPUAssignedAt:             tt.assignedAt,
				HypervisorProcessIdentity: HypervisorProcessIdentity{HypervisorPID: tt.pid},
			}}))

			claimed, err := m.vgpuAssignmentClaimedByLiveInstance("requester", devicePath)
			if tt.wantErr != "" {
				require.ErrorContains(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.False(t, claimed)
		})
	}
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
