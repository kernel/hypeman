package instances

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/kernel/hypeman/lib/devices"
	"github.com/kernel/hypeman/lib/paths"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testVGPUProfile   = "NVIDIA L40S-2Q"
	testVFDevicePath  = "/sys/bus/pci/devices/0000:82:00.4"
	testVFProfileType = "1148"
)

func saveTestVGPUInstance(t *testing.T, m *manager, id string) *metadata {
	t.Helper()
	require.NoError(t, m.ensureDirectories(id))
	meta := &metadata{StoredMetadata: StoredMetadata{Id: id, Name: id, GPUProfile: testVGPUProfile}}
	require.NoError(t, m.saveMetadata(meta))
	return meta
}

func TestReleaseStoredVGPUKeepsClaimWhenResetFails(t *testing.T) {
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			return errors.New("reset failed")
		},
	}
	meta := saveTestVGPUInstance(t, m, "claimed")
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(meta))

	err := m.releaseStoredVGPUPersisted(context.Background(), meta)
	require.ErrorContains(t, err, "reset failed")
	stored, loadErr := m.loadMetadata(meta.Id)
	require.NoError(t, loadErr)
	assert.Equal(t, testVFDevicePath, stored.GPUDevicePath)
}

func TestCreateCleanupResetsWhileClaimIsStillPersisted(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	meta := saveTestVGPUInstance(t, m, "failed-create")
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(meta))
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		onDisk, err := m.loadMetadata(meta.Id)
		require.NoError(t, err)
		assert.Equal(t, testVFDevicePath, onDisk.GPUDevicePath)
		return errors.New("reset failed")
	}

	assert.True(t, m.cleanupCreateVGPU(context.Background(), &meta.StoredMetadata))
	require.NoError(t, m.deleteInstanceData(meta.Id))
	_, err := m.loadMetadata(meta.Id)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestCreateCleanupPreservesClaimForLiveVMM(t *testing.T) {
	m := &manager{paths: paths.New(t.TempDir())}
	meta := saveTestVGPUInstance(t, m, "live-create")
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = testVFDevicePath
	meta.HypervisorProcessIdentity.Set(os.Getpid())
	require.NoError(t, m.saveMetadata(meta))
	m.destroyVGPU = func(context.Context, devices.VGPUAssignment) error {
		t.Fatal("must not reset a VF owned by a live VMM")
		return nil
	}

	assert.False(t, m.cleanupCreateVGPU(context.Background(), &meta.StoredMetadata))
	stored, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	assert.Equal(t, testVFDevicePath, stored.GPUDevicePath)
}

func TestStartCleanupRemovesClaimAfterResetFailure(t *testing.T) {
	m := &manager{
		paths: paths.New(t.TempDir()),
		destroyVGPU: func(context.Context, devices.VGPUAssignment) error {
			return errors.New("reset failed")
		},
	}
	meta := saveTestVGPUInstance(t, m, "failed-start")
	rollback := *meta
	meta.GPUFramework = devices.VGPUFrameworkVendorVFIO
	meta.GPUDevicePath = testVFDevicePath
	require.NoError(t, m.saveMetadata(meta))

	m.cleanupStartVGPU(context.Background(), &meta.StoredMetadata, rollback)
	stored, err := m.loadMetadata(meta.Id)
	require.NoError(t, err)
	assert.Empty(t, stored.GPUDevicePath)
	assert.Equal(t, testVGPUProfile, stored.GPUProfile)
}

func TestStoredVGPUDevicePath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, testVFDevicePath, storedVGPUDevicePath(&StoredMetadata{
		GPUDevicePath: testVFDevicePath,
		GPUMdevUUID:   "legacy-uuid",
	}))
	assert.Equal(t, "/sys/bus/mdev/devices/legacy-uuid", storedVGPUDevicePath(&StoredMetadata{
		GPUMdevUUID: "legacy-uuid",
	}))
	assert.Empty(t, storedVGPUDevicePath(&StoredMetadata{}))
}

func TestSelectVendorVFIOVFFailsClosedOnClaimedVF(t *testing.T) {
	vfs := []devices.VirtualFunction{{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}}
	profiles := map[string][]devices.VGPUProfileType{
		"0000:82:00.4": {{TypeName: testVFProfileType, Name: testVGPUProfile, FramebufferMB: 2048}},
	}
	_, _, err := selectVendorVFIOVF(vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: profiles,
		claims:       []StoredMetadata{{GPUDevicePath: testVFDevicePath}},
	}, testVGPUProfile, nil)
	require.ErrorContains(t, err, "no available VF")
}

func testVFProfiles(addresses ...string) map[string][]devices.VGPUProfileType {
	profiles := make(map[string][]devices.VGPUProfileType, len(addresses))
	for _, address := range addresses {
		profiles[address] = []devices.VGPUProfileType{{TypeName: testVFProfileType, Name: testVGPUProfile, FramebufferMB: 2048}}
	}
	return profiles
}

func pickFirst(int) int { return 0 }

func pickLast(n int) int { return n - 1 }

func TestSelectVendorVFIOVFSkipsQuarantinedVF(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	quarantined := map[string]struct{}{"0000:82:00.4": {}}

	vf, _, err := selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs, profilesByVF: testVFProfiles("0000:82:00.4", "0000:82:00.5"), quarantined: quarantined}, testVGPUProfile, pickFirst)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", vf)

	_, _, err = selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs[:1], profilesByVF: testVFProfiles("0000:82:00.4"), quarantined: quarantined}, testVGPUProfile, pickFirst)
	require.ErrorContains(t, err, "no available VF")
}

func TestSelectVendorVFIOVFAvoidsGPUWithQuarantinedVF(t *testing.T) {
	// Both GPUs are idle; GPU 82 sorts first by name but carries a
	// quarantined VF, so the clean card wins.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0"},
	}
	quarantined := map[string]struct{}{"0000:82:00.4": {}}

	vf, _, err := selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs, profilesByVF: testVFProfiles("0000:82:00.4", "0000:82:00.5", "0000:e3:00.4"), quarantined: quarantined}, testVGPUProfile, pickFirst)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.4", vf)
}

func TestSelectVendorVFIOVFPicksAmongEquivalentFreeVFs(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	var offered int
	pick := func(n int) int {
		offered = n
		return n - 1
	}

	vf, _, err := selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs, profilesByVF: testVFProfiles("0000:82:00.4", "0000:82:00.5")}, testVGPUProfile, pick)
	require.NoError(t, err)
	assert.Equal(t, 2, offered)
	assert.Equal(t, "0000:82:00.5", vf)
}

func TestSelectVendorVFIOVFRandomizesOnlyAmongCleanVFs(t *testing.T) {
	// The dirty VF is still a candidate of last resort but must never be
	// offered to the tiebreak while a clean sibling exists.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.6", ParentGPU: "0000:82:00.0"},
	}
	var offered int
	pick := func(n int) int {
		offered = n
		return n - 1
	}

	vf, _, err := selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs, profilesByVF: testVFProfiles("0000:82:00.5", "0000:82:00.6")}, testVGPUProfile, pick)
	require.NoError(t, err)
	assert.Equal(t, 2, offered)
	assert.Equal(t, "0000:82:00.6", vf)

	vf, _, err = selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs[:1], profilesByVF: testVFProfiles("0000:82:00.4")}, testVGPUProfile, pickLast)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.4", vf)
}

func TestSelectVendorVFIOVFPrefersGPUWithKnownLoad(t *testing.T) {
	// GPU 82 carries a claim whose profile is no longer creatable anywhere,
	// so its load is unknown; GPU e3 has a known 2 GB claim. Known load wins
	// even though it is non-zero.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: "999"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.5", ParentGPU: "0000:e3:00.0"},
	}
	profile := devices.VGPUProfileType{TypeName: testVFProfileType, Name: testVGPUProfile, FramebufferMB: 2048}
	profiles := map[string][]devices.VGPUProfileType{
		"0000:82:00.5": {profile},
		"0000:e3:00.5": {profile},
	}
	claims := []StoredMetadata{
		{GPUProfile: "NVIDIA L40S-48Q", GPUDevicePath: testVFDevicePath},
		{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4"},
	}

	vf, profileType, err := selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs, profilesByVF: profiles, claims: claims}, testVGPUProfile, nil)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.5", vf)
	assert.Equal(t, testVFProfileType, profileType)

	// With no alternative, the GPU with unknown load is still used.
	vf, _, err = selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs[:2], profilesByVF: profiles, claims: claims[:1]}, testVGPUProfile, nil)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", vf)
}

const (
	testLargeVGPUProfile = "NVIDIA L40S-4Q"
	testLargeProfileType = "1150"
)

var (
	testSmallProfile = devices.VGPUProfileType{TypeName: testVFProfileType, Name: testVGPUProfile, FramebufferMB: 2048}
	testLargeProfile = devices.VGPUProfileType{TypeName: testLargeProfileType, Name: testLargeVGPUProfile, FramebufferMB: 4096}
)

// testSupported reports both profiles as supported on every listed GPU.
func testSupported(gpus ...string) map[string]map[string]devices.VGPUProfileType {
	supported := make(map[string]map[string]devices.VGPUProfileType, len(gpus))
	for _, gpu := range gpus {
		supported[gpu] = map[string]devices.VGPUProfileType{
			testVGPUProfile:      testSmallProfile,
			testLargeVGPUProfile: testLargeProfile,
		}
	}
	return supported
}

func TestSelectVendorVFIOVFOffersSupportedProfileItsGPUHides(t *testing.T) {
	// Both GPUs are busy enough that the driver hides 4Q from every
	// creatable list, but both have supported it. The less loaded GPU wins.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.6", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.5", ParentGPU: "0000:e3:00.0"},
	}
	claims := []StoredMetadata{
		{GPUProfile: testVGPUProfile, GPUDevicePath: testVFDevicePath},
		{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:82:00.5"},
		{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4"},
	}
	c := vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: testVFProfiles("0000:82:00.6", "0000:e3:00.5"),
		supported:    testSupported("0000:82:00.0", "0000:e3:00.0"),
		claims:       claims,
	}

	vf, profileType, err := selectVendorVFIOVF(c, testLargeVGPUProfile, nil)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.5", vf)
	assert.Equal(t, testLargeProfileType, profileType)

	// Without a remembered profile the request fails.
	c.supported = nil
	_, _, err = selectVendorVFIOVF(c, testLargeVGPUProfile, nil)
	require.ErrorContains(t, err, "is not creatable on any VF")
}

func TestSelectVendorVFIOVFPrefersGPUThatAdvertisesProfile(t *testing.T) {
	// GPU 82 is idle but hides 4Q in this snapshot; GPU e3 is busier but
	// still advertises it. A certain placement beats a lighter load.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.5", ParentGPU: "0000:e3:00.0"},
	}
	profiles := testVFProfiles("0000:82:00.4")
	profiles["0000:e3:00.5"] = []devices.VGPUProfileType{testSmallProfile, testLargeProfile}
	claims := []StoredMetadata{{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4"}}

	vf, _, err := selectVendorVFIOVF(vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: profiles,
		supported:    testSupported("0000:82:00.0", "0000:e3:00.0"),
		claims:       claims,
	}, testLargeVGPUProfile, nil)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.5", vf)
}

func TestSelectVendorVFIOVFOffersHiddenProfileOnlyWhereSupported(t *testing.T) {
	// GPU 82 is idle but has never advertised 4Q, so it is not offered it.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.5", ParentGPU: "0000:e3:00.0"},
	}
	vf, _, err := selectVendorVFIOVF(vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: testVFProfiles("0000:82:00.4", "0000:e3:00.5"),
		supported:    testSupported("0000:e3:00.0"),
		claims:       []StoredMetadata{{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4"}},
	}, testLargeVGPUProfile, nil)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.5", vf)
}

func TestSelectVendorVFIOVFSkipsUnreadableVFForHiddenProfile(t *testing.T) {
	// VF .4's creatable types could not be read, so it is absent from the
	// listing. Its readable sibling says the GPU still has placements, but
	// that says nothing about the unreadable VF itself.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	vf, _, err := selectVendorVFIOVF(vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: testVFProfiles("0000:82:00.5"),
		supported:    testSupported("0000:82:00.0"),
	}, testLargeVGPUProfile, pickFirst)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", vf)
}

func TestSelectVendorVFIOVFCountsHiddenProfileClaimsAsKnownLoad(t *testing.T) {
	// GPU 82 runs one 4Q that no VF advertises any more; GPU e3 runs three
	// 2Q. The remembered 4Q makes 82's load known and lighter (4 GB vs 6 GB),
	// instead of ranking 82 last for unknown load.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testLargeProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.5", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.6", ParentGPU: "0000:e3:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:e3:00.7", ParentGPU: "0000:e3:00.0"},
	}
	c := vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: testVFProfiles("0000:82:00.5", "0000:e3:00.7"),
		supported:    testSupported("0000:82:00.0", "0000:e3:00.0"),
		claims: []StoredMetadata{
			{GPUProfile: testLargeVGPUProfile, GPUDevicePath: testVFDevicePath},
			{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.4"},
			{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.5"},
			{GPUProfile: testVGPUProfile, GPUDevicePath: "/sys/bus/pci/devices/0000:e3:00.6"},
		},
	}

	vf, _, err := selectVendorVFIOVF(c, testVGPUProfile, nil)
	require.NoError(t, err)
	assert.Equal(t, "0000:82:00.5", vf)

	c.supported = nil
	vf, _, err = selectVendorVFIOVF(c, testVGPUProfile, nil)
	require.NoError(t, err)
	assert.Equal(t, "0000:e3:00.7", vf, "without the remembered profile 82's load is unknown")
}

func TestSelectVendorVFIOVFSkipsGPUThatAdvertisesNothing(t *testing.T) {
	// A free VF that advertises no profile at all means the GPU has no
	// placement left for any size, so it is not worth asking the driver.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	_, _, err := selectVendorVFIOVF(vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: map[string][]devices.VGPUProfileType{"0000:82:00.5": {}},
		supported:    testSupported("0000:82:00.0"),
		claims:       []StoredMetadata{{GPUProfile: testVGPUProfile, GPUDevicePath: testVFDevicePath}},
	}, testLargeVGPUProfile, nil)
	require.ErrorContains(t, err, "no creatable vGPU profiles on any VF")
}

func TestSelectVendorVFIOVFOffersHiddenProfileOnlyOnCleanVF(t *testing.T) {
	// GPU 82 hides 4Q and its only free VF is dirty. A configured VF lists
	// nothing on L40S, but a hidden profile is only offered on a clean VF
	// regardless of what the dirty one lists; claimVGPU resets unclaimed
	// dirty VFs and selects again when this fails.
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType},
	}
	_, _, err := selectVendorVFIOVF(vendorVFIOCandidates{
		vfs:          vfs,
		profilesByVF: testVFProfiles("0000:82:00.5"),
		supported:    testSupported("0000:82:00.0"),
		claims:       []StoredMetadata{{GPUProfile: testVGPUProfile, GPUDevicePath: testVFDevicePath}},
	}, testLargeVGPUProfile, nil)
	require.ErrorContains(t, err, "is not creatable on any VF")
}
