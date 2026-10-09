package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/images"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/require"
)

func testMacImage() *images.Image {
	size := int64(64 << 30)
	return &images.Image{Platform: "darwin/arm64", SizeBytes: &size, MacOS: &images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{2}, MAC: "02:00:00:00:00:01", CPUs: 4, Memory: 8 << 30}}
}
func TestMacOSRequestDefaultsAndRejections(t *testing.T) {
	caps := hypervisor.Capabilities{SupportsMacOSBoot: true}
	req := CreateInstanceRequest{}
	require.NoError(t, prepareMacOSCreate(&req, testMacImage(), caps))
	require.Equal(t, int64(8<<30), req.Size)
	require.Equal(t, 4, req.Vcpus)
	require.Equal(t, int64(64<<30), req.OverlaySize)
	require.True(t, req.SkipGuestAgent)
	for _, r := range []CreateInstanceRequest{
		{Env: map[string]string{"X": "Y"}}, {Cmd: []string{"sh"}}, {HotplugSize: 1}, {OverlaySize: 1}, {DiskIOBps: 1}, {Size: 1 << 30}, {Volumes: []VolumeAttachment{{VolumeID: "v"}}},
	} {
		original := r
		require.ErrorIs(t, prepareMacOSCreate(&r, testMacImage(), caps), ErrInvalidRequest)
		require.Equal(t, original, r)
	}
	require.ErrorIs(t, prepareMacOSCreate(&CreateInstanceRequest{}, testMacImage(), hypervisor.Capabilities{}), ErrInvalidRequest)
}
func TestMacOSGuestAgentOptIn(t *testing.T) {
	caps := hypervisor.Capabilities{SupportsMacOSBoot: true}
	image := testMacImage()
	image.MacOS.GuestAgent = true
	request := CreateInstanceRequest{}
	require.NoError(t, validateMacOSCreate(request, image, caps))
	applyMacOSDefaults(&request, image)
	require.False(t, request.SkipGuestAgent)
	request = CreateInstanceRequest{SkipGuestAgent: true}
	applyMacOSDefaults(&request, image)
	require.True(t, request.SkipGuestAgent)
}

func TestMacOSAgentReadinessSeparateFromRunning(t *testing.T) {
	for _, tc := range []struct {
		name                     string
		declared, skipped, ready bool
	}{
		{"legacy image", false, false, false},
		{"disabled", true, true, false},
		{"not ready", true, false, false},
		{"ready", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := testMacImage()
			image.MacOS.GuestAgent = tc.declared
			stored := StoredMetadata{Id: "mac", MacOS: image.MacOS, SkipGuestAgent: tc.skipped}
			now := time.Now().UTC()
			calls := 0
			m := &manager{now: func() time.Time { return now }, guestAgentReadyProbe: func(context.Context, *StoredMetadata) bool {
				calls++
				return tc.ready
			}}
			require.Equal(t, StateRunning, deriveRunningState(&stored))
			hydrated := m.hydrateBootMarkersFromLogs(context.Background(), &stored)
			require.Equal(t, tc.ready, hydrated)
			require.Equal(t, StateRunning, deriveRunningState(&stored))
			require.Nil(t, stored.ProgramStartedAt, "macOS does not invent a Linux workload marker")
			if tc.ready {
				require.Equal(t, &now, stored.GuestAgentReadyAt)
			} else {
				require.Nil(t, stored.GuestAgentReadyAt)
			}
			if tc.declared && !tc.skipped {
				require.Equal(t, 1, calls)
			} else {
				require.Zero(t, calls)
			}
		})
	}
}

func TestMacOSAgentReadyMarkerPersisted(t *testing.T) {
	p := paths.New(t.TempDir())
	now := time.Now().UTC()
	m := &manager{paths: p, now: func() time.Time { return now }, guestAgentReadyProbe: func(context.Context, *StoredMetadata) bool { return true }}
	require.NoError(t, m.ensureDirectories("mac"))
	image := testMacImage()
	image.MacOS.GuestAgent = true
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{Id: "mac", DataDir: p.InstanceDir("mac"), MacOS: image.MacOS}}))
	m.persistBootMarkers(context.Background(), "mac")
	meta, err := m.loadMetadata("mac")
	require.NoError(t, err)
	require.Equal(t, &now, meta.GuestAgentReadyAt)
	require.Nil(t, meta.ProgramStartedAt)
	require.True(t, meta.MacOS.GuestAgent)
}

func TestMacOSAgentReadyMarkerPersistedByPublicRead(t *testing.T) {
	p := paths.New(t.TempDir())
	now := time.Now().UTC()
	m := &manager{paths: p, now: func() time.Time { return now }, guestAgentReadyProbe: func(context.Context, *StoredMetadata) bool { return true }}
	require.NoError(t, m.ensureDirectories("mac"))
	socket := filepath.Join(p.InstanceDir("mac"), "vz.sock")
	require.NoError(t, os.WriteFile(socket, nil, 0600))
	image := testMacImage()
	image.MacOS.GuestAgent = true
	stored := StoredMetadata{Id: "mac", DataDir: p.InstanceDir("mac"), SocketPath: socket, MacOS: image.MacOS, HypervisorType: hypervisor.TypeVZ, CreatedAt: now}
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))
	m.storeCachedHypervisorState("mac", hypervisor.StateRunning)

	inst, err := m.GetInstance(context.Background(), "mac")
	require.NoError(t, err)
	require.NotNil(t, inst.GuestAgentReadyAt)
	meta, err := m.loadMetadata("mac")
	require.NoError(t, err)
	require.NotNil(t, meta.GuestAgentReadyAt, "readiness must reach metadata through the normal read path")
}

func TestMacOSBootConfigNoLinuxDevices(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	inst := &Instance{StoredMetadata: StoredMetadata{Id: "mac", DataDir: p.InstanceDir("mac"), Size: 8 << 30, Vcpus: 4, MacOS: testMacImage().MacOS, NetworkEnabled: true, MAC: "02:00:00:00:00:01"}}
	cfg, err := m.buildHypervisorConfig(context.Background(), inst, nil, nil)
	require.NoError(t, err)
	require.Equal(t, hypervisor.BootModeMacOS, cfg.BootMode)
	require.Len(t, cfg.Disks, 1)
	require.False(t, cfg.Disks[0].Readonly)
	require.Empty(t, cfg.KernelPath)
	require.Empty(t, cfg.InitrdPath)
	require.Empty(t, cfg.SerialLogPath)
	require.False(t, cfg.GuestMemory.EnableBalloon)
	require.Equal(t, StateRunning, deriveRunningState(&inst.StoredMetadata))
	require.Nil(t, inst.ProgramStartedAt)
}
func TestMacOSIdentityAdmissionSkipsDirectoriesWithoutMetadata(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	stored := StoredMetadata{Id: "new", DataDir: p.InstanceDir("new"), MacOS: testMacImage().MacOS}
	require.NoError(t, m.ensureDirectories("orphan"))
	require.NoError(t, m.ensureDirectories("new"))
	require.NoError(t, m.checkMacOSIdentityAvailable(context.Background(), &stored))

	require.NoError(t, os.WriteFile(p.InstanceMetadata("orphan"), []byte("{not json"), 0600))
	require.Error(t, m.checkMacOSIdentityAvailable(context.Background(), &stored))
}

func TestMacOSPreservedIdentityAdmission(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	require.NoError(t, m.ensureDirectories("active"))
	socket := filepath.Join(p.InstanceDir("active"), "vz.sock")
	require.NoError(t, os.WriteFile(socket, nil, 0600))
	stored := StoredMetadata{Id: "active", DataDir: p.InstanceDir("active"), SocketPath: socket, MacOS: testMacImage().MacOS, HypervisorType: hypervisor.TypeVZ, CreatedAt: time.Now()}
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))
	m.storeCachedHypervisorState("active", hypervisor.StateRunning)
	other := stored
	other.Id = "other"
	err := m.checkMacOSIdentityAvailable(context.Background(), &other)
	require.True(t, errors.Is(err, ErrInsufficientResources))
	require.NoError(t, m.checkMacOSIdentityAvailable(context.Background(), &stored))
	require.NoError(t, os.Remove(socket))
	require.NoError(t, m.checkMacOSIdentityAvailable(context.Background(), &other))
	require.ErrorIs(t, stored.rejectMacOS("snapshot"), ErrInvalidRequest)
}
