//go:build linux

package instances

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	otelmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func newVGPUAllocationManager(t *testing.T, vfs []devices.VirtualFunction) *manager {
	t.Helper()
	return &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			profiles := make(map[string][]devices.VGPUProfileType, len(vfs))
			for _, vf := range vfs {
				profiles[vf.PCIAddress] = []devices.VGPUProfileType{{
					TypeName:      testVFProfileType,
					Name:          testVGPUProfile,
					FramebufferMB: 2048,
				}}
			}
			return profiles, nil
		},
		configureVGPU: func(context.Context, string, string) error { return nil },
	}
}

func TestConcurrentVGPUClaimsUseDistinctVFs(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	saveTestVGPUInstance(t, m, "instance-a")
	saveTestVGPUInstance(t, m, "instance-b")

	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for _, id := range []string{"instance-a", "instance-b"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			meta, err := m.loadMetadata(id)
			if err == nil {
				_, err = m.claimVGPU(context.Background(), meta, testVGPUProfile)
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}

	claims := make(map[string]struct{})
	for _, id := range []string{"instance-a", "instance-b"} {
		meta, err := m.loadMetadata(id)
		require.NoError(t, err)
		claims[meta.GPUDevicePath] = struct{}{}
	}
	assert.Len(t, claims, 2)
}

func TestVGPUClaimSkipsQuarantinedVF(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	m.quarantinedVFs = func() (map[string]struct{}, error) {
		return map[string]struct{}{"0000:82:00.4": {}}, nil
	}
	m.pickVFIndex = pickFirst
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
}

func TestVGPUClaimFailsClosedWhenVFHealthUnavailable(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	m.quarantinedVFs = func() (map[string]struct{}, error) {
		return nil, errors.New("VF health state unavailable: read failed")
	}
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.ErrorContains(t, err, "VF health state unavailable")
	stored, loadErr := m.loadMetadata("new")
	require.NoError(t, loadErr)
	assert.Empty(t, stored.GPUDevicePath, "placement must not claim while quarantine state is unknown")
}

func TestVGPUClaimRecordsClaimTime(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	claimedAt := time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return claimedAt }
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error { return nil }
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	stored, err := m.loadMetadata("new")
	require.NoError(t, err)
	require.NotNil(t, stored.GPUClaimedAt)
	assert.True(t, claimedAt.Equal(*stored.GPUClaimedAt))

	require.NoError(t, m.releaseStoredVGPUPersisted(context.Background(), stored))
	stored, err = m.loadMetadata("new")
	require.NoError(t, err)
	assert.Nil(t, stored.GPUClaimedAt, "release must clear the assignment identity with the claim")
}

func TestVGPUClaimUsesLeastLoadedGPU(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	claimed := saveTestVGPUInstance(t, m, "claimed")
	claimed.GPUFramework = devices.VGPUFrameworkVendorVFIO
	claimed.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(claimed))
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress)
}

func TestVGPUClaimResetsDirtySameTypeVFBeforeClaim(t *testing.T) {
	// The leftover type matches the requested profile, so configure would
	// no-op; the reset must happen at claim time, before the claim exists.
	vfs := []devices.VirtualFunction{{
		PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType,
	}}
	m := newVGPUAllocationManager(t, vfs)
	var resets []string
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		onDisk, err := m.loadMetadata("new")
		require.NoError(t, err)
		assert.Empty(t, onDisk.GPUDevicePath, "reset must run before the claim is persisted")
		resets = append(resets, assignment.DevicePath)
		return nil
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", device.VFAddress)
	assert.Equal(t, []string{testVFDevicePath}, resets)
}

func TestVGPUClaimFailsWhenDirtyVFStillInUse(t *testing.T) {
	vfs := []devices.VirtualFunction{{
		PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType,
	}}
	m := newVGPUAllocationManager(t, vfs)
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		return errors.New("vendor VFIO vGPU on VF 0000:82:00.4 is still in use")
	}
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.ErrorContains(t, err, "repair dirty VF 0000:82:00.4 before claim")
	stored, loadErr := m.loadMetadata("new")
	require.NoError(t, loadErr)
	assert.Empty(t, stored.GPUDevicePath, "a failed repair must not leave a claim")
}

func TestVGPUClaimRepairsDirtyVFWhenProfileNotAdvertised(t *testing.T) {
	// The dirty VF's leftover type consumes the framebuffer that would make
	// the requested profile creatable, so no VF advertises it until the VF
	// is reset.
	dirty := true
	var resets []string
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			vf := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
			if dirty {
				vf.Allocated = true
				vf.ProfileType = "999"
			}
			return devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{vf}, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			if dirty {
				return map[string][]devices.VGPUProfileType{}, nil
			}
			return map[string][]devices.VGPUProfileType{
				"0000:82:00.4": {{TypeName: testVFProfileType, Name: testVGPUProfile, FramebufferMB: 2048}},
			}, nil
		},
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			resets = append(resets, assignment.DevicePath)
			dirty = false
			return nil
		},
		configureVGPU: func(context.Context, string, string) error { return nil },
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", device.VFAddress)
	assert.Equal(t, []string{testVFDevicePath}, resets)
	stored, err := m.loadMetadata("new")
	require.NoError(t, err)
	assert.Equal(t, testVFDevicePath, stored.GPUDevicePath)
}

func TestVGPUClaimRepairSkipsClaimedVFs(t *testing.T) {
	vfs := []devices.VirtualFunction{{
		PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: "999",
	}}
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			return map[string][]devices.VGPUProfileType{}, nil
		},
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			t.Fatal("must not reset a VF claimed by another instance")
			return nil
		},
	}
	owner := saveTestVGPUInstance(t, m, "owner")
	owner.GPUFramework = devices.VGPUFrameworkVendorVFIO
	owner.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(owner))
	meta := saveTestVGPUInstance(t, m, "new")

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.Error(t, err)
}

func TestVGPUCrashAfterClaimIsReconciled(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	meta := saveTestVGPUInstance(t, m, "mid-create")
	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)

	var destroyed []devices.VGPUAssignment
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		destroyed = append(destroyed, assignment)
		return nil
	}
	m.reconcileVGPUDevices = func(context.Context, map[string]struct{}) error { return nil }
	m.ReconcileVGPUs(context.Background())

	require.Equal(t, []devices.VGPUAssignment{{
		Framework: devices.VGPUFrameworkVendorVFIO, DevicePath: device.SysfsPath, InstanceID: "mid-create",
	}}, destroyed)
	stored, err := m.loadMetadata("mid-create")
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath)
}

func TestVGPUClaimPrefersCleanVFOnSameGPU(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		t.Fatal("a clean VF must be claimed without resetting a dirty sibling")
		return nil
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
}

func TestVGPUClaimFallsBackWhenDirtyVFRefusesReset(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
	}
	m := newVGPUAllocationManager(t, vfs)
	// Both VFs are equally ranked; pin the tiebreak so the fallback order is fixed.
	m.pickVFIndex = pickFirst
	var resets []string
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		resets = append(resets, assignment.DevicePath)
		if assignment.DevicePath == testVFDevicePath {
			return errors.New("vendor VFIO vGPU on VF 0000:82:00.4 is still in use")
		}
		return nil
	}
	meta := saveTestVGPUInstance(t, m, "new")

	device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
	assert.Equal(t, []string{testVFDevicePath, "/sys/bus/pci/devices/0000:82:00.5"}, resets)
}

// newHiddenProfileManager returns a manager on a two-GPU host whose free VFs
// advertise both profiles until hide4Q is set, after which they advertise
// only 2Q, as the driver does once a GPU holds 12 vGPUs.
func newHiddenProfileManager(t *testing.T, hide4Q *bool) *manager {
	t.Helper()
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0"},
	}
	return &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			profiles := make(map[string][]devices.VGPUProfileType, len(vfs))
			for _, vf := range vfs {
				profiles[vf.PCIAddress] = []devices.VGPUProfileType{testSmallProfile}
				if !*hide4Q {
					profiles[vf.PCIAddress] = append(profiles[vf.PCIAddress], testLargeProfile)
				}
			}
			return profiles, nil
		},
		pickVFIndex: pickFirst,
	}
}

func TestVGPUClaimPlacesProfileItsGPUHides(t *testing.T) {
	hide4Q := false
	m := newHiddenProfileManager(t, &hide4Q)
	var configured []string
	m.configureVGPU = func(_ context.Context, vfAddress, profileType string) error {
		configured = append(configured, vfAddress+"="+profileType)
		return nil
	}
	// An earlier claim on the idle host records that both GPUs support 4Q.
	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "earlier"), testVGPUProfile)
	require.NoError(t, err)
	require.Equal(t, []string{"0000:82:00.4=" + testVFProfileType}, configured)

	hide4Q = true
	configured = nil
	meta := saveTestVGPUInstance(t, m, "large")
	device, err := m.claimVGPU(context.Background(), meta, testLargeVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress)
	assert.Equal(t, testLargeProfileType, device.ProfileType)
	assert.Equal(t, []string{"0000:e3:00.4=" + testLargeProfileType}, configured)
	stored, err := m.loadMetadata("large")
	require.NoError(t, err)
	assert.Equal(t, "/sys/bus/pci/devices/0000:e3:00.4", stored.GPUDevicePath)
}

func TestVGPUClaimTriesNextGPUWhenDriverRefusesHiddenProfile(t *testing.T) {
	hide4Q := true
	m := newHiddenProfileManager(t, &hide4Q)
	m.supportedVGPUProfiles = testSupported("0000:82:00.0", "0000:e3:00.0")
	reader := otelmetric.NewManualReader()
	metrics, err := newInstanceMetrics(otelmetric.NewMeterProvider(otelmetric.WithReader(reader)).Meter("test"), nil, m)
	require.NoError(t, err)
	m.metrics = metrics
	var configured []string
	m.configureVGPU = func(_ context.Context, vfAddress, profileType string) error {
		configured = append(configured, vfAddress)
		if vfAddress == "0000:82:00.4" {
			return errDriverRejected(vfAddress)
		}
		return nil
	}

	device, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", device.VFAddress)
	assert.Equal(t, []string{"0000:82:00.4", "0000:e3:00.4"}, configured, "a refusal rules out the whole GPU, not just the VF")

	var rm metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &rm))
	rejections, ok := findMetric(t, rm, "hypeman_instances_vgpu_driver_rejections_total").Data.(metricdata.Sum[int64])
	require.True(t, ok)
	require.Len(t, rejections.DataPoints, 1)
	assert.Equal(t, int64(1), rejections.DataPoints[0].Value)
	gpu, ok := rejections.DataPoints[0].Attributes.Value("gpu")
	require.True(t, ok)
	assert.Equal(t, "0000:82:00.0", gpu.AsString())
}

func TestVGPUClaimFailsWhenDriverRefusesHiddenProfileEverywhere(t *testing.T) {
	hide4Q := true
	m := newHiddenProfileManager(t, &hide4Q)
	m.supportedVGPUProfiles = testSupported("0000:82:00.0", "0000:e3:00.0")
	var configured []string
	m.configureVGPU = func(_ context.Context, vfAddress, _ string) error {
		configured = append(configured, vfAddress)
		return errDriverRejected(vfAddress)
	}

	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.ErrorContains(t, err, "is not creatable on any VF")
	require.ErrorContains(t, err, "driver refused it")
	assert.Equal(t, []string{"0000:82:00.4", "0000:e3:00.4"}, configured)
	stored, loadErr := m.loadMetadata("large")
	require.NoError(t, loadErr)
	assert.Empty(t, stored.GPUDevicePath)
}

func TestVGPUClaimFailsWhenDriverRefusesAdvertisedProfileEverywhere(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	var configured []string
	m.configureVGPU = func(_ context.Context, vfAddress, _ string) error {
		configured = append(configured, vfAddress)
		return errDriverRejected(vfAddress)
	}

	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "new"), testVGPUProfile)
	require.ErrorContains(t, err, "is not creatable on any VF")
	require.ErrorContains(t, err, "driver refused it")
	assert.Len(t, configured, 2)
}

func TestVGPUClaimFailsOnHiddenProfileConfigureErrorOtherThanRejection(t *testing.T) {
	hide4Q := true
	m := newHiddenProfileManager(t, &hide4Q)
	m.supportedVGPUProfiles = testSupported("0000:82:00.0", "0000:e3:00.0")
	var configured []string
	m.configureVGPU = func(_ context.Context, vfAddress, _ string) error {
		configured = append(configured, vfAddress)
		return errors.New("VF health state unavailable: read failed")
	}

	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.ErrorContains(t, err, "VF health state unavailable")
	assert.NotContains(t, err.Error(), "not creatable", "only a driver rejection says the GPU is out of placements")
	assert.Equal(t, []string{"0000:82:00.4"}, configured)
}

func TestVGPUClaimResetsDirtyVFSelectedAfterDriverRefusal(t *testing.T) {
	// GPU 82 refuses the hidden 4Q, so the claim moves to GPU e3, whose only
	// free VF is dirty with a leftover 4Q and still open. It must go through
	// the reset like any other dirty VF instead of being claimed as is.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testLargeProfileType},
	}
	var events []string
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			return map[string][]devices.VGPUProfileType{
				"0000:82:00.4": {testSmallProfile},
				"0000:e3:00.4": {},
			}, nil
		},
		supportedVGPUProfiles: testSupported("0000:82:00.0", "0000:e3:00.0"),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			events = append(events, "reset "+assignment.DevicePath)
			return errors.New("vendor VFIO vGPU on VF 0000:e3:00.4 is still in use")
		},
		configureVGPU: func(_ context.Context, vfAddress, _ string) error {
			events = append(events, "configure "+vfAddress)
			if vfAddress == "0000:82:00.4" {
				return errDriverRejected(vfAddress)
			}
			return nil
		},
		pickVFIndex: pickFirst,
	}

	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.ErrorContains(t, err, "repair dirty VF 0000:e3:00.4 before claim")
	assert.Equal(t, []string{"configure 0000:82:00.4", "reset /sys/bus/pci/devices/0000:e3:00.4"}, events)
	stored, loadErr := m.loadMetadata("large")
	require.NoError(t, loadErr)
	assert.Empty(t, stored.GPUDevicePath)
}

func TestVGPUClaimResetsDirtySiblingHoldingHiddenPlacement(t *testing.T) {
	// GPU 82 hides 4Q, and its last 4Q placement is held by an unclaimed
	// dirty VF, for example one configured by a claim that failed to save.
	// The clean sibling is tried first; when the driver refuses it, the dirty
	// VF is reset and the write retried rather than giving up on the GPU.
	dirty := true
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			leftover := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
			if dirty {
				leftover.Allocated, leftover.ProfileType = true, testLargeProfileType
			}
			return devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{
				leftover,
				{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
			}, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			return map[string][]devices.VGPUProfileType{
				"0000:82:00.4": {},
				"0000:82:00.5": {testSmallProfile},
			}, nil
		},
		supportedVGPUProfiles: testSupported("0000:82:00.0"),
		pickVFIndex:           pickFirst,
	}
	var events []string
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		events = append(events, "reset "+assignment.DevicePath)
		dirty = false
		return nil
	}
	m.configureVGPU = func(_ context.Context, vfAddress, _ string) error {
		events = append(events, "configure "+vfAddress)
		if dirty {
			return errDriverRejected(vfAddress)
		}
		return nil
	}

	device, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
	assert.Equal(t, []string{
		"configure 0000:82:00.5",
		"reset /sys/bus/pci/devices/0000:82:00.4",
		"configure 0000:82:00.5",
	}, events)
}

func TestVGPUClaimKeepsClaimedSiblingWhenDriverRefuses(t *testing.T) {
	// GPU 82's configured sibling belongs to another instance, so a refusal
	// must not reset it to free a placement.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	var events []string
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			return map[string][]devices.VGPUProfileType{
				"0000:82:00.4": {},
				"0000:82:00.5": {testSmallProfile},
			}, nil
		},
		supportedVGPUProfiles: testSupported("0000:82:00.0"),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			events = append(events, "reset "+assignment.DevicePath)
			return nil
		},
		configureVGPU: func(_ context.Context, vfAddress, _ string) error {
			events = append(events, "configure "+vfAddress)
			return errDriverRejected(vfAddress)
		},
	}
	running := saveTestVGPUInstance(t, m, "running")
	running.GPUFramework = devices.VGPUFrameworkVendorVFIO
	running.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(running))

	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.ErrorContains(t, err, "driver refused it on GPU 0000:82:00.0")
	assert.Equal(t, []string{"configure 0000:82:00.5"}, events)
}

func TestVGPUClaimResetsDirtyVFsOnOtherGPUAfterHiddenProfileRefused(t *testing.T) {
	// GPU 41 hides 4Q and refuses it. GPU 82 is filled by an unclaimed dirty
	// VF, so its free VF lists nothing and is no candidate. The hidden
	// candidate on 41 satisfied the first selection, so the dirty VF must
	// still be reset before the claim gives up, without resetting the VF
	// another instance claims or retrying GPU 41.
	dirty := true
	var events []string
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			leftover := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
			if dirty {
				leftover.Allocated, leftover.ProfileType = true, testVFProfileType
			}
			return devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{
				{PCIAddress: "0000:41:00.4", ParentGPU: "0000:41:00.0"},
				leftover,
				{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
				{PCIAddress: "0000:82:00.6", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
			}, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			profiles := map[string][]devices.VGPUProfileType{
				"0000:41:00.4": {testSmallProfile},
				"0000:82:00.4": {},
				"0000:82:00.5": {},
				"0000:82:00.6": {},
			}
			if !dirty {
				profiles["0000:82:00.5"] = []devices.VGPUProfileType{testSmallProfile}
			}
			return profiles, nil
		},
		supportedVGPUProfiles: testSupported("0000:41:00.0", "0000:82:00.0"),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			events = append(events, "reset "+assignment.DevicePath)
			dirty = false
			return nil
		},
		configureVGPU: func(_ context.Context, vfAddress, _ string) error {
			events = append(events, "configure "+vfAddress)
			if vfAddress == "0000:41:00.4" {
				return errDriverRejected(vfAddress)
			}
			return nil
		},
		pickVFIndex: pickFirst,
	}
	running := saveTestVGPUInstance(t, m, "running")
	running.GPUFramework = devices.VGPUFrameworkVendorVFIO
	running.GPUDevicePath = "/sys/bus/pci/devices/0000:82:00.6"
	require.NoError(t, m.saveMetadata(running))

	device, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", device.VFAddress)
	assert.Equal(t, []string{
		"configure 0000:41:00.4",
		"reset " + testVFDevicePath,
		"configure 0000:82:00.5",
	}, events)
}

func TestVGPUClaimProbesRepairedVFOnceWhenDriverRefuses(t *testing.T) {
	// The only candidate is a dirty VF left at 4Q on a GPU that hides 4Q. It
	// is reset, then probed. A refusal must not count that same VF as a dirty
	// sibling to reset and retry.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testLargeProfileType},
	}
	var events []string
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, vfs, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			return map[string][]devices.VGPUProfileType{"0000:82:00.4": {}}, nil
		},
		supportedVGPUProfiles: testSupported("0000:82:00.0"),
		destroyVGPU: func(_ context.Context, assignment devices.VGPUAssignment) error {
			events = append(events, "reset "+assignment.DevicePath)
			return nil
		},
		configureVGPU: func(_ context.Context, vfAddress, _ string) error {
			events = append(events, "configure "+vfAddress)
			return errDriverRejected(vfAddress)
		},
	}

	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.ErrorContains(t, err, "driver refused it on GPU 0000:82:00.0")
	assert.Equal(t, []string{"reset " + testVFDevicePath, "configure 0000:82:00.4"}, events)
}

func TestVGPUClaimRemembersProfilesListedAfterDirtyReset(t *testing.T) {
	// The first claim only sees the GPU's profiles after resetting a dirty
	// VF that consumed its framebuffer. 4Q must still be remembered from that
	// listing once the GPU later hides it.
	dirty := true
	hide4Q := false
	m := &manager{
		paths: paths.New(t.TempDir()),
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			leftover := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
			if dirty {
				leftover.Allocated, leftover.ProfileType = true, "999"
			}
			return devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{
				leftover,
				{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
			}, nil
		},
		vendorVFIOProfiles: func(vfs []devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			profiles := make(map[string][]devices.VGPUProfileType, len(vfs))
			for _, vf := range vfs {
				switch {
				case dirty:
					profiles[vf.PCIAddress] = []devices.VGPUProfileType{}
				case hide4Q:
					profiles[vf.PCIAddress] = []devices.VGPUProfileType{testSmallProfile}
				default:
					profiles[vf.PCIAddress] = []devices.VGPUProfileType{testSmallProfile, testLargeProfile}
				}
			}
			return profiles, nil
		},
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			dirty = false
			return nil
		},
		configureVGPU: func(context.Context, string, string) error { return nil },
		pickVFIndex:   pickFirst,
	}
	_, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "small"), testVGPUProfile)
	require.NoError(t, err)

	hide4Q = true
	_, err = m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "large"), testLargeVGPUProfile)
	require.NoError(t, err)
}

func TestVGPUClaimConfiguresBeforeReleasingAllocationLock(t *testing.T) {
	// GPU 82 has one placement left, but its free VFs still list 2Q. The
	// first claim must take that placement before releasing the allocation
	// lock, so the second is refused by the driver and moves to GPU e3,
	// instead of both claims landing on GPU 82 and one failing later.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.5", ParentGPU: "0000:e3:00.0"},
	}
	m := newVGPUAllocationManager(t, vfs)
	m.pickVFIndex = pickFirst
	running := saveTestVGPUInstance(t, m, "running")
	running.GPUFramework = devices.VGPUFrameworkVendorVFIO
	running.GPUDevicePath = "/sys/bus/pci/devices/0000:e3:00.4"
	require.NoError(t, m.saveMetadata(running))
	placementsLeft := map[string]int{"0000:82:00.0": 1, "0000:e3:00.0": 1}
	m.configureVGPU = func(_ context.Context, vfAddress, _ string) error {
		vf, ok := vfByAddress(vfs, vfAddress)
		require.True(t, ok)
		if placementsLeft[vf.ParentGPU] == 0 {
			return errDriverRejected(vfAddress)
		}
		placementsLeft[vf.ParentGPU]--
		return nil
	}

	first, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "first"), testVGPUProfile)
	require.NoError(t, err)
	second, err := m.claimVGPU(context.Background(), saveTestVGPUInstance(t, m, "second"), testVGPUProfile)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", first.VFAddress)
	assert.Equal(t, "0000:e3:00.5", second.VFAddress)
	assert.Equal(t, map[string]int{"0000:82:00.0": 0, "0000:e3:00.0": 0}, placementsLeft)
}

func TestVGPUClaimResetsConfiguredVFWhenClaimSaveFails(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	m := newVGPUAllocationManager(t, vfs)
	var resets []string
	m.destroyVGPU = func(_ context.Context, assignment devices.VGPUAssignment) error {
		resets = append(resets, assignment.DevicePath)
		return nil
	}
	meta := saveTestVGPUInstance(t, m, "new")
	require.NoError(t, os.RemoveAll(m.paths.InstanceDir("new")))

	_, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
	require.ErrorContains(t, err, "save vGPU claim")
	assert.Equal(t, []string{testVFDevicePath}, resets)
	assert.Empty(t, meta.GPUDevicePath)
}

func errDriverRejected(vfAddress string) error {
	return fmt.Errorf("configure vGPU on VF %s: %w: invalid argument", vfAddress, devices.ErrVGPUTypeRejected)
}
