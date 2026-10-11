package instances

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
		{Env: map[string]string{"X": "Y"}}, {Cmd: []string{"sh"}}, {HotplugSize: 1}, {OverlaySize: 1}, {DiskIOBps: 1}, {Size: 1 << 30}, {Size: (4 << 30) + 1}, {Vcpus: 1}, {Volumes: []VolumeAttachment{{VolumeID: "v"}}},
	} {
		original := r
		require.ErrorIs(t, prepareMacOSCreate(&r, testMacImage(), caps), ErrInvalidRequest)
		require.Equal(t, original, r)
	}
	require.ErrorIs(t, prepareMacOSCreate(&CreateInstanceRequest{}, testMacImage(), hypervisor.Capabilities{}), ErrInvalidRequest)
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

func TestMacOSMACAdmission(t *testing.T) {
	p := paths.New(t.TempDir())
	m := &manager{paths: p}
	require.NoError(t, m.ensureDirectories("active"))
	socket := filepath.Join(p.InstanceDir("active"), "vz.sock")
	require.NoError(t, os.WriteFile(socket, nil, 0600))
	active := StoredMetadata{Id: "active", DataDir: p.InstanceDir("active"), SocketPath: socket, MacOS: testMacImage().MacOS, HypervisorType: hypervisor.TypeVZ, CreatedAt: time.Now(), NetworkEnabled: true, MAC: "02:00:00:00:00:01"}
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: active}))
	m.storeCachedHypervisorState("active", hypervisor.StateRunning)

	other := StoredMetadata{Id: "other", DataDir: p.InstanceDir("other"), NetworkEnabled: true, MacOS: &images.MacOSImage{HardwareModel: []byte{1}, MachineIdentifier: []byte{3}, MAC: "02-00-00-00-00-01"}}
	require.ErrorIs(t, m.checkMacOSIdentityAvailable(context.Background(), &other), ErrInsufficientResources, "a different identity on the same MAC is refused")

	other.NetworkEnabled = false
	require.NoError(t, m.checkMacOSIdentityAvailable(context.Background(), &other), "a guest without a NIC holds no MAC")
	other.NetworkEnabled = true
	other.MacOS.MAC = "02:00:00:00:00:02"
	require.NoError(t, m.checkMacOSIdentityAvailable(context.Background(), &other))
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
	require.ErrorIs(t, (&manager{}).admitGuestOperation(&stored, "snapshot schedules"), ErrInvalidRequest)
	require.ErrorIs(t, (&manager{}).admitGuestOperation(&stored, "fork snapshot"), ErrInvalidRequest)
}

func TestMacOSSocketPathLimit(t *testing.T) {
	dir := "/" + strings.Repeat("d", maxMacOSSocketPath-len("/vz.vsock")-1)
	stored := &StoredMetadata{MacOS: testMacImage().MacOS, HypervisorType: hypervisor.TypeVZ, SocketPath: dir + "/vz.sock", VsockSocket: dir + "/vz.vsock"}
	require.Len(t, stored.VsockSocket, maxMacOSSocketPath)
	require.NoError(t, checkMacOSSocketPaths(stored))

	stored.VsockSocket += "x"
	require.ErrorIs(t, checkMacOSSocketPaths(stored), ErrInvalidRequest, "a vsock path that cannot bind is refused before boot")
	stored.VsockSocket = dir + "/vz.vsock"
	stored.SocketPath = dir + "/" + strings.Repeat("s", len("vz.vsock")+1)
	require.ErrorIs(t, checkMacOSSocketPaths(stored), ErrInvalidRequest)
	stored.HypervisorType = "test-starter"
	require.NoError(t, checkMacOSSocketPaths(stored), "only the VZ shim binds these sockets")
}
