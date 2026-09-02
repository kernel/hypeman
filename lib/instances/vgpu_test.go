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
