package resources

import (
	"context"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAllocatableVGPUSlotsExcludesQuarantinedVFs(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4"},
		{PCIAddress: "0000:82:00.5", Allocated: true},
		{PCIAddress: "0000:82:00.6"},
	}
	quarantined := []devices.VFHealthRecord{{VFAddress: "0000:82:00.4"}}

	assert.Equal(t, 1, allocatableVGPUSlots(devices.VGPUFrameworkVendorVFIO, vfs, quarantined, false))
	assert.Zero(t, allocatableVGPUSlots(devices.VGPUFrameworkVendorVFIO, vfs, quarantined, true))
	assert.Equal(t, 2, allocatableVGPUSlots(devices.VGPUFrameworkMdev, vfs, quarantined, false))
}

func TestReserveAllocationUsesAllocatableGPUSlots(t *testing.T) {
	status := &GPUResourceStatus{
		Mode:             string(devices.GPUModeVGPU),
		TotalSlots:       4,
		UsedSlots:        1,
		AllocatableSlots: 0,
	}
	setGPUStatusProvider(func(context.Context) *GPUResourceStatus { return status })
	t.Cleanup(func() { setGPUStatusProvider(nil) })

	mgr := NewManager(&config.Config{}, paths.New(t.TempDir()))
	ctx := context.Background()

	err := mgr.ValidateAllocation(ctx, 0, 0, 0, 0, 0, 0, true)
	require.ErrorContains(t, err, "no allocatable vgpu slots")

	status.AllocatableSlots = 1
	require.NoError(t, mgr.ReserveAllocation(ctx, "pending-a", 0, 0, 0, 0, 0, 0, true))
	err = mgr.ReserveAllocation(ctx, "pending-b", 0, 0, 0, 0, 0, 0, true)
	require.ErrorContains(t, err, "no allocatable vgpu slots")

	mgr.FinishAllocation("pending-a")
	require.NoError(t, mgr.ReserveAllocation(ctx, "pending-b", 0, 0, 0, 0, 0, 0, true))
}
