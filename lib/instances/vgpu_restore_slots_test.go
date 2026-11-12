package instances

import (
	"context"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/hypervisor"
	"github.com/kernel/hypeman/lib/hypervisor/qemu"
	"github.com/stretchr/testify/require"
)

func TestVGPURestoreSlotsDoNotThrottleCPUQEMU(t *testing.T) {
	m := &manager{vgpuRestoreSlots: make(chan struct{}, 1)}
	gpu := &StoredMetadata{HypervisorType: hypervisor.TypeQEMU, HypervisorVersion: qemu.VGPUSnapshotVersion, GPUProfile: "NVIDIA L40S-3Q"}
	release, err := m.acquireInstanceRestoreSlot(t.Context(), gpu)
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	cpuRelease, err := m.acquireInstanceRestoreSlot(ctx, &StoredMetadata{HypervisorType: hypervisor.TypeQEMU})
	require.NoError(t, err)
	cpuRelease()
	_, err = m.acquireInstanceRestoreSlot(ctx, gpu)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	release()
	release, err = m.acquireInstanceRestoreSlot(t.Context(), gpu)
	require.NoError(t, err)
	release()
}

func TestVGPURestoreSlotsDoNotReuseTheGenericQEMUPool(t *testing.T) {
	require.NotEqual(t, sharedRestoreSlots(hypervisor.TypeQEMU, 2), sharedVGPURestoreSlots(2))
}

func TestGuestReconfigureIsBoundedOnlyForVGPURestores(t *testing.T) {
	gpu := &StoredMetadata{HypervisorType: hypervisor.TypeQEMU, HypervisorVersion: qemu.VGPUSnapshotVersion, GPUProfile: "NVIDIA L40S-3Q"}
	ctx, cancel := guestReconfigureContext(t.Context(), gpu)
	defer cancel()
	deadline, ok := ctx.Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(vgpuRestoreAgentTimeout), deadline, time.Second)

	ctx, cancel = guestReconfigureContext(t.Context(), &StoredMetadata{HypervisorType: hypervisor.TypeFirecracker})
	defer cancel()
	_, ok = ctx.Deadline()
	require.False(t, ok)
}
