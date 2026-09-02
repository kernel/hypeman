package resources

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kernel/hypeman/cmd/api/config"
	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const testVFQuarantineThreshold = 2

// initVFHealthForTest points the VF health store at a state file for one test
// and detaches it from disk again afterwards.
func initVFHealthForTest(t *testing.T, state []byte) {
	t.Helper()
	path := paths.New(t.TempDir()).VFHealthState()
	if state != nil {
		require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
		require.NoError(t, os.WriteFile(path, state, 0o644))
	}
	err := devices.InitVFHealth(path, testVFQuarantineThreshold)
	if state == nil {
		require.NoError(t, err)
	}
	t.Cleanup(func() { require.NoError(t, devices.InitVFHealth("", testVFQuarantineThreshold)) })
}

func TestGetVGPUStatusFailsClosedWhenVFHealthIsUnavailable(t *testing.T) {
	initVFHealthForTest(t, []byte("not json"))

	status, err := getVGPUStatus(context.Background(), devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{{PCIAddress: "0000:82:00.4"}})
	assert.Zero(t, status.AllocatableSlots)
	assert.Zero(t, status.QuarantinedSlots)
	require.ErrorContains(t, err, "VF health state unavailable")
}

func TestGetVGPUStatusReportsQuarantinedSlots(t *testing.T) {
	initVFHealthForTest(t, nil)
	for _, instance := range []string{"instance-1", "instance-2"} {
		_, err := devices.ReportVFInitFailure(devices.VFInitFailureReport{VFAddress: "0000:82:00.4", InstanceID: instance})
		require.NoError(t, err)
	}

	status, err := getVGPUStatus(context.Background(), devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4"},
		{PCIAddress: "0000:82:00.5", Allocated: true},
		{PCIAddress: "0000:82:00.6"},
	})
	require.NoError(t, err)
	assert.Equal(t, 3, status.TotalSlots)
	assert.Equal(t, 1, status.UsedSlots)
	assert.Equal(t, 1, status.AllocatableSlots)
	assert.Equal(t, 1, status.QuarantinedSlots)
}

func TestReserveAllocationUsesAllocatableGPUSlots(t *testing.T) {
	status := &GPUResourceStatus{
		Mode:             string(devices.GPUModeVGPU),
		TotalSlots:       4,
		UsedSlots:        1,
		AllocatableSlots: 0,
	}
	setGPUStatusProvider(func(context.Context) (*GPUResourceStatus, error) { return status, nil })
	t.Cleanup(func() { setGPUStatusProvider(nil) })

	mgr := NewManager(&config.Config{}, paths.New(t.TempDir()))
	ctx := context.Background()

	err := mgr.ValidateAllocation(ctx, 0, 0, 0, 0, 0, 0, true)
	require.ErrorContains(t, err, "no allocatable vgpu slots")

	setGPUStatusProvider(func(context.Context) (*GPUResourceStatus, error) {
		return status, errors.New("VF health state unavailable: read failed")
	})
	err = mgr.ValidateAllocation(ctx, 0, 0, 0, 0, 0, 0, true)
	require.ErrorContains(t, err, "vGPU placement is disabled: VF health state unavailable")
	statusMgr, _, _ := monitoringTestManager(t)
	full, err := statusMgr.GetFullStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, full.GPU)
	assert.Equal(t, "VF health state unavailable: read failed", full.GPU.PlacementDisabledReason)
	setGPUStatusProvider(func(context.Context) (*GPUResourceStatus, error) { return status, nil })
	full, err = statusMgr.GetFullStatus(ctx)
	require.NoError(t, err)
	assert.Empty(t, full.GPU.PlacementDisabledReason)

	status.AllocatableSlots = 1
	require.NoError(t, mgr.ReserveAllocation(ctx, "pending-a", 0, 0, 0, 0, 0, 0, true))
	err = mgr.ReserveAllocation(ctx, "pending-b", 0, 0, 0, 0, 0, 0, true)
	require.ErrorContains(t, err, "no allocatable vgpu slots")

	mgr.FinishAllocation("pending-a")
	require.NoError(t, mgr.ReserveAllocation(ctx, "pending-b", 0, 0, 0, 0, 0, 0, true))
}
