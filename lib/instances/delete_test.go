package instances

import (
	"context"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/network"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestWaitForProcessExit_ReapsZombieChild(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())

	exited := WaitForProcessExit(cmd.Process.Pid, 500*time.Millisecond)
	require.True(t, exited, "zombie child should be detected/reaped as exited")
}

func TestWaitForProcessExit_EPERMProcessIsAlive(t *testing.T) {
	t.Parallel()
	if syscall.Kill(1, 0) == nil {
		t.Skip("running as root")
	}

	assert.False(t, WaitForProcessExit(1, 100*time.Millisecond))
}

func TestWaitForProcessExit_TimesOutForRunningProcess(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("sleep", "2")
	require.NoError(t, cmd.Start())
	defer func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	exited := WaitForProcessExit(cmd.Process.Pid, 100*time.Millisecond)
	assert.False(t, exited, "running process should time out")
}

func TestResolveDeleteStopTimeout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		stored StoredMetadata
		want   int
	}{
		{name: "default", want: deleteGracefulShutdownTimeout},
		{name: "invalid", stored: StoredMetadata{StopTimeout: -1}, want: deleteGracefulShutdownTimeout},
		{name: "below cap", stored: StoredMetadata{StopTimeout: 1}, want: 1},
		{name: "above cap", stored: StoredMetadata{StopTimeout: 30}, want: deleteGracefulShutdownTimeout},
		{name: "mdev default", stored: StoredMetadata{GPUMdevUUID: "0d7c3b4e-6f2a-4d77-8f1b-2a9c5e4d1f00"}, want: vfioDeleteGracefulShutdownTimeout},
		{name: "vendor vfio default", stored: StoredMetadata{GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4"}, want: vfioDeleteGracefulShutdownTimeout},
		{name: "passthrough default", stored: StoredMetadata{Devices: []string{"gpu-0"}}, want: vfioDeleteGracefulShutdownTimeout},
		{name: "vfio below cap", stored: StoredMetadata{GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4", StopTimeout: 3}, want: 3},
		{name: "vfio above cap", stored: StoredMetadata{GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.4", StopTimeout: 30}, want: vfioDeleteGracefulShutdownTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			assert.Equal(t, tt.want, resolveDeleteStopTimeout(&tt.stored))
		})
	}
}

type holdRecordingNetworkManager struct {
	network.Manager
	held []string
}

func (n *holdRecordingNetworkManager) HoldReleasedIP(_ context.Context, ip string) {
	n.held = append(n.held, ip)
}

func TestDeleteHoldsGuestIP(t *testing.T) {
	m := createTestManager(t, ResourceLimits{})
	nm := &holdRecordingNetworkManager{Manager: m.networkManager}
	m.networkManager = nm

	const id = "delete-hold"
	stored := StoredMetadata{Id: id, Name: id, NetworkEnabled: true, IP: "10.100.0.42", MAC: "02:00:00:00:00:42", HypervisorType: hypervisor.TypeCloudHypervisor}
	require.NoError(t, os.MkdirAll(m.paths.InstanceDir(id), 0755))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: stored}))

	require.NoError(t, m.deleteInstance(context.Background(), id))
	require.Equal(t, []string{"10.100.0.42"}, nm.held)
	require.NoDirExists(t, m.paths.InstanceDir(id))
}
