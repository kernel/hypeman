//go:build linux

package instances

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A VMM whose post-boot metadata save failed has no persisted PID, but it
// still holds its control-socket listener. The reconciler must protect its
// assignment past the startup grace period instead of releasing the device
// out from under the live VM.
func TestReconcileVGPUsProtectsSocketOwnerWithoutPersistedPID(t *testing.T) {
	t.Parallel()

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
	stale := time.Now().UTC().Add(-VGPUAssignmentStartupGracePeriod - time.Minute)
	require.NoError(t, m.ensureDirectories(id))
	require.NoError(t, m.saveMetadata(&metadata{StoredMetadata: StoredMetadata{
		Id:            id,
		SocketPath:    socketPath,
		GPUFramework:  devices.VGPUFrameworkMdev,
		GPUDevicePath: "/sys/bus/mdev/devices/test-mdev",
		GPUMdevUUID:   "test-mdev",
		GPUAssignedAt: &stale,
	}}))

	m.ReconcileVGPUs(t.Context())

	assert.Empty(t, destroyed, "a live socket owner must block the release even with a nil persisted PID")
	assert.Contains(t, protected, "/sys/bus/mdev/devices/test-mdev")
	stored, err := m.loadMetadata(id)
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/mdev/devices/test-mdev", stored.GPUDevicePath)
}
