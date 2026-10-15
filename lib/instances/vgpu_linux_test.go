//go:build linux

package instances

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
			return map[string][]devices.VGPUProfileType{"0000:82:00.4": {}}, nil
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
	require.ErrorIs(t, err, ErrInsufficientResources)
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

// fakeVGPUHost models a vendor VFIO host whose GPUs run out of framebuffer
// before they run out of VFs: a free VF advertises the profile only while its
// parent GPU has framebuffer left, and every allocated VF consumes one unit.
type fakeVGPUHost struct {
	mu          sync.Mutex
	vfs         []devices.VirtualFunction
	free        map[string]int
	unreadable  map[string]bool
	quarantined map[string]struct{}
	resetFails  map[string]bool
}

func (h *fakeVGPUHost) discover() (devices.VGPUFramework, []devices.VirtualFunction, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return devices.VGPUFrameworkVendorVFIO, append([]devices.VirtualFunction(nil), h.vfs...), nil
}

func (h *fakeVGPUHost) profiles(vfs []devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make(map[string][]devices.VGPUProfileType, len(vfs))
	for _, vf := range vfs {
		if h.unreadable[vf.PCIAddress] {
			continue
		}
		out[vf.PCIAddress] = []devices.VGPUProfileType{}
		if !vf.Allocated && h.free[vf.ParentGPU] > 0 {
			out[vf.PCIAddress] = []devices.VGPUProfileType{{TypeName: testVFProfileType, Name: testVGPUProfile, FramebufferMB: 2048}}
		}
	}
	return out, nil
}

func (h *fakeVGPUHost) configure(address string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	for i := range h.vfs {
		if h.vfs[i].PCIAddress != address {
			continue
		}
		if h.free[h.vfs[i].ParentGPU] == 0 {
			return false
		}
		h.free[h.vfs[i].ParentGPU]--
		h.vfs[i].Allocated = true
		h.vfs[i].ProfileType = testVFProfileType
		return true
	}
	return false
}

func (h *fakeVGPUHost) destroy(_ context.Context, assignment devices.VGPUAssignment) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	address := filepath.Base(assignment.DevicePath)
	if h.resetFails[address] {
		return errors.New("VF in use")
	}
	for i := range h.vfs {
		if h.vfs[i].PCIAddress == address && h.vfs[i].Allocated {
			h.vfs[i].Allocated = false
			h.vfs[i].ProfileType = "0"
			h.free[h.vfs[i].ParentGPU]++
		}
	}
	return nil
}

func (h *fakeVGPUHost) manager(t *testing.T, dir string) *manager {
	t.Helper()
	return &manager{
		paths:              paths.New(dir),
		discoverVGPU:       h.discover,
		vendorVFIOProfiles: h.profiles,
		destroyVGPU:        h.destroy,
		quarantinedVFs: func() (map[string]struct{}, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			out := make(map[string]struct{}, len(h.quarantined))
			for address := range h.quarantined {
				out[address] = struct{}{}
			}
			return out, nil
		},
	}
}

// TestCheckVGPUCapacityRefusalImpliesClaimRefusal checks, across random host
// states, that whenever the pre-check refuses, a claim on the same state also
// fails with ErrInsufficientResources.
func TestCheckVGPUCapacityRefusalImpliesClaimRefusal(t *testing.T) {
	rng := rand.New(rand.NewPCG(1, 2))
	gpus := []string{"0000:82:00.0", "0000:e3:00.0"}
	refusals := 0
	const iterations = 1000
	for iter := 0; iter < iterations; iter++ {
		host := &fakeVGPUHost{
			free:        map[string]int{},
			unreadable:  map[string]bool{},
			quarantined: map[string]struct{}{},
			resetFails:  map[string]bool{},
		}
		var claimedAddresses []string
		for g, gpu := range gpus {
			host.free[gpu] = rng.IntN(3)
			for v := 0; v < 1+rng.IntN(4); v++ {
				vf := devices.VirtualFunction{PCIAddress: fmt.Sprintf("0000:%02x:00.%d", 0x82+g*0x61, 4+v), ParentGPU: gpu, ProfileType: "0"}
				// 0 free, 1 claimed and configured, 2 dirty (allocated, unclaimed),
				// 3 claimed but not yet configured.
				switch rng.IntN(4) {
				case 1:
					vf.Allocated, vf.ProfileType = true, testVFProfileType
					claimedAddresses = append(claimedAddresses, vf.PCIAddress)
				case 2:
					vf.Allocated, vf.ProfileType = true, testVFProfileType
				case 3:
					claimedAddresses = append(claimedAddresses, vf.PCIAddress)
				}
				if rng.IntN(8) == 0 {
					host.quarantined[vf.PCIAddress] = struct{}{}
				}
				if rng.IntN(10) == 0 {
					host.unreadable[vf.PCIAddress] = true
				}
				if rng.IntN(4) == 0 {
					host.resetFails[vf.PCIAddress] = true
				}
				host.vfs = append(host.vfs, vf)
			}
		}

		m := host.manager(t, t.TempDir())
		for i, address := range claimedAddresses {
			owner := saveTestVGPUInstance(t, m, fmt.Sprintf("owner-%d", i))
			owner.GPUFramework = devices.VGPUFrameworkVendorVFIO
			owner.GPUDevicePath = "/sys/bus/pci/devices/" + address
			require.NoError(t, m.saveMetadata(owner))
		}

		err := m.checkVGPUCapacity(testVGPUProfile)
		if err == nil {
			continue
		}
		require.ErrorIs(t, err, ErrInsufficientResources)
		refusals++
		meta := saveTestVGPUInstance(t, m, "new")
		_, claimErr := m.claimVGPU(context.Background(), meta, testVGPUProfile)
		require.ErrorIs(t, claimErr, ErrInsufficientResources, "iteration %d: pre-check refused but claim returned %v", iter, claimErr)
	}
	require.Greater(t, refusals, iterations/20, "generator should produce full hosts")
}

// TestCheckVGPUCapacityConcurrentWithClaimsAndReleases runs pre-checks,
// claims, and releases in parallel against one host. Run with -race.
func TestCheckVGPUCapacityConcurrentWithClaimsAndReleases(t *testing.T) {
	host := &fakeVGPUHost{
		free:        map[string]int{"0000:82:00.0": 1, "0000:e3:00.0": 1},
		unreadable:  map[string]bool{},
		quarantined: map[string]struct{}{},
		resetFails:  map[string]bool{},
	}
	for g, gpu := range []string{"0000:82:00.0", "0000:e3:00.0"} {
		for v := 0; v < 4; v++ {
			host.vfs = append(host.vfs, devices.VirtualFunction{PCIAddress: fmt.Sprintf("0000:%02x:00.%d", 0x82+g*0x61, 4+v), ParentGPU: gpu, ProfileType: "0"})
		}
	}
	m := host.manager(t, t.TempDir())

	var liveMu sync.Mutex
	live := map[string]string{}
	var refused, claimed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				func() {
					id := fmt.Sprintf("w%d-%d", w, i)
					meta := saveTestVGPUInstance(t, m, id)
					defer m.deleteInstanceData(id)
					if err := m.checkVGPUCapacity(testVGPUProfile); err != nil {
						assert.ErrorIs(t, err, ErrInsufficientResources)
						refused.Add(1)
						return
					}
					device, err := m.claimVGPU(context.Background(), meta, testVGPUProfile)
					if err != nil {
						assert.ErrorIs(t, err, ErrInsufficientResources)
						return
					}
					if !host.configure(device.VFAddress) {
						// Another claim used the last framebuffer first; roll back.
						assert.NoError(t, m.releaseStoredVGPUPersisted(context.Background(), meta))
						return
					}
					claimed.Add(1)
					liveMu.Lock()
					if owner, ok := live[device.VFAddress]; ok {
						t.Errorf("VF %s assigned to %s and %s", device.VFAddress, owner, id)
					}
					live[device.VFAddress] = id
					liveMu.Unlock()

					time.Sleep(10 * time.Millisecond)

					liveMu.Lock()
					delete(live, device.VFAddress)
					liveMu.Unlock()
					assert.NoError(t, m.releaseStoredVGPUPersisted(context.Background(), meta))
				}()
			}
		}(w)
	}
	wg.Wait()
	t.Logf("pre-check refusals: %d, configured claims: %d", refused.Load(), claimed.Load())
	assert.Positive(t, claimed.Load())
	assert.Positive(t, refused.Load())

	host.mu.Lock()
	defer host.mu.Unlock()
	for _, vf := range host.vfs {
		assert.False(t, vf.Allocated, "VF %s still allocated after all releases", vf.PCIAddress)
	}
	assert.Equal(t, 1, host.free["0000:82:00.0"])
	assert.Equal(t, 1, host.free["0000:e3:00.0"])
}
