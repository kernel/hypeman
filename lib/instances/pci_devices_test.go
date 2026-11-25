package instances

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/stretchr/testify/require"
)

func TestRecoveryLeavesInheritedPCIDeviceWithOwner(t *testing.T) {
	m, _ := setupTestManager(t)
	const owner, fork, dev = "source", "interrupted", "dev-1"
	createStoppedSnapshotSourceFixture(t, m, fork, fork, m.defaultHypervisor)
	meta, err := m.loadMetadata(fork)
	require.NoError(t, err)
	meta.ForkPending = true
	meta.Devices = []string{dev}
	require.NoError(t, m.saveMetadata(meta))
	data := writeAttachedTestDevice(t, m, dev, owner)

	m.RecoverInterruptedForks(t.Context())
	require.NoDirExists(t, m.paths.InstanceDir(fork))
	after, err := os.ReadFile(m.paths.DeviceMetadata(dev))
	require.NoError(t, err)
	require.JSONEq(t, string(data), string(after))
}

func TestDeleteSourceKeepsPCIDeviceUsedByFork(t *testing.T) {
	m, _ := setupTestManager(t)
	const source, fork, dev = "stopped-source", "running-child", "dev-1"
	for _, id := range []string{source, fork} {
		createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
		meta, err := m.loadMetadata(id)
		require.NoError(t, err)
		meta.Devices = []string{dev}
		if id == fork {
			meta.HypervisorType = lifecycleNoopHypervisorType
			writeLifecycleNoopSocket(t, meta.SocketPath, hypervisor.StateRunning)
			t.Cleanup(func() { lifecycleNoopHypervisorStates.Delete(meta.SocketPath) })
		}
		require.NoError(t, m.saveMetadata(meta))
	}
	before := writeAttachedTestDevice(t, m, dev, source)
	err := m.DeleteInstance(t.Context(), source)
	require.ErrorIs(t, err, devices.ErrInUse)
	require.FileExists(t, m.paths.InstanceMetadata(source))
	after, err := os.ReadFile(m.paths.DeviceMetadata(dev))
	require.NoError(t, err)
	require.JSONEq(t, string(before), string(after))

	meta, err := m.loadMetadata(fork)
	require.NoError(t, err)
	require.NoError(t, os.Remove(meta.SocketPath))
	require.NoError(t, m.DeleteInstance(t.Context(), source))
	require.NoDirExists(t, m.paths.InstanceDir(source))
}

func writeAttachedTestDevice(t *testing.T, m *manager, dev, owner string) []byte {
	t.Helper()
	data, err := json.Marshal(devices.Device{Id: dev, AttachedTo: &owner, BoundToVFIO: true, PCIAddress: "test-device"})
	require.NoError(t, err)
	require.NoError(t, os.MkdirAll(m.paths.DeviceDir(dev), 0755))
	require.NoError(t, os.WriteFile(m.paths.DeviceMetadata(dev), data, 0644))
	return data
}

type busyDevice struct{ devices.Manager }

func (busyDevice) ReleaseInstanceDevice(context.Context, string, string) error {
	return errors.New("device busy")
}

// Nothing retries a release for an instance that is gone, and a device still
// attached to it cannot be given to another instance until the next start.
func TestDeleteFreesPCIDeviceWhenReleaseFails(t *testing.T) {
	m, _ := setupTestManager(t)
	const id, dev = "deleted", "dev-1"
	createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.Devices = []string{dev}
	require.NoError(t, m.saveMetadata(meta))
	writeAttachedTestDevice(t, m, dev, id)
	m.deviceManager = busyDevice{m.deviceManager}

	require.NoError(t, m.deleteInstance(t.Context(), id))
	device, err := m.deviceManager.GetDevice(t.Context(), dev)
	require.NoError(t, err)
	require.Nil(t, device.AttachedTo)
}

func TestRecoveryKeepsForkWhenPCIReleaseFails(t *testing.T) {
	m, _ := setupTestManager(t)
	const id = "pending-pci"
	createStoppedSnapshotSourceFixture(t, m, id, id, m.defaultHypervisor)
	meta, err := m.loadMetadata(id)
	require.NoError(t, err)
	meta.ForkPending = true
	meta.Devices = []string{"dev-1"}
	require.NoError(t, m.saveMetadata(meta))
	m.deviceManager = busyDevice{m.deviceManager}

	m.RecoverInterruptedForks(t.Context())
	got, err := m.loadMetadata(id)
	require.NoError(t, err)
	require.Equal(t, meta.ForkPending, got.ForkPending)
}
