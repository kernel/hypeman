//go:build linux

package instances

import (
	"context"
	"net"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestReconcileVGPUsProtectsSocketOwnerWithoutPersistedPID(t *testing.T) {
	socketPath := filepath.Join(t.TempDir(), "test.sock")
	listener, err := net.Listen("unix", socketPath)
	require.NoError(t, err)
	defer listener.Close()

	var destroyed []devices.VGPUAssignment
	var protected map[string]struct{}
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			destroyed = append(destroyed, assignment)
			return nil
		},
		reconcileVGPUDevices: func(_ context.Context, p map[string]struct{}, _ bool) error {
			protected = p
			return nil
		},
	}
	const id = "pid-save-failed"
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            id,
		SocketPath:    socketPath,
		GPUFramework:  devices.VGPUFrameworkMdev,
		GPUDevicePath: "/sys/bus/mdev/devices/test-mdev",
		GPUMdevUUID:   "test-mdev",
	}}))

	m.ReconcileVGPUs(t.Context())

	assert.Empty(t, destroyed)
	assert.Contains(t, protected, "/sys/bus/mdev/devices/test-mdev")
	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/mdev/devices/test-mdev", stored.GPUDevicePath)
}
