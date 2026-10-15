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

func TestSelectVendorVFIOVFReportsCapacityAsInsufficientResources(t *testing.T) {
	vf := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
	tests := []struct {
		name       string
		candidates vendorVFIOCandidates
	}{
		{
			name:       "no profile creatable on any VF",
			candidates: vendorVFIOCandidates{vfs: []devices.VirtualFunction{vf}, profilesByVF: map[string][]devices.VGPUProfileType{vf.PCIAddress: {}}},
		},
		{
			name: "requested profile no longer creatable",
			candidates: vendorVFIOCandidates{vfs: []devices.VirtualFunction{vf}, profilesByVF: map[string][]devices.VGPUProfileType{
				vf.PCIAddress: {{TypeName: "1147", Name: "NVIDIA L40S-1Q", FramebufferMB: 1024}},
			}},
		},
		{
			name: "every VF claimed",
			candidates: vendorVFIOCandidates{
				vfs:          []devices.VirtualFunction{vf},
				profilesByVF: testVFProfiles(vf.PCIAddress),
				claims:       []StoredMetadata{{GPUDevicePath: testVFDevicePath}},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := selectVendorVFIOVF(tt.candidates, testVGPUProfile, nil)
			require.ErrorIs(t, err, ErrInsufficientResources)
			assert.ErrorIs(t, wrapCreateVGPUErr(testVGPUProfile, err), ErrInsufficientResources)
		})
	}
}

func TestSelectVendorVFIOVFDoesNotReportUnreadableVFsAsCapacity(t *testing.T) {
	vfs := []devices.VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
	}
	tests := []struct {
		name     string
		profiles map[string][]devices.VGPUProfileType
		claims   []StoredMetadata
	}{
		{name: "no VF readable", profiles: map[string][]devices.VGPUProfileType{}},
		{name: "readable VFs advertise nothing", profiles: map[string][]devices.VGPUProfileType{"0000:82:00.4": {}}},
		{
			name:     "only claimed VF readable",
			profiles: testVFProfiles("0000:82:00.4"),
			claims:   []StoredMetadata{{GPUDevicePath: testVFDevicePath}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := selectVendorVFIOVF(vendorVFIOCandidates{vfs: vfs, profilesByVF: tt.profiles, claims: tt.claims}, testVGPUProfile, nil)
			require.Error(t, err)
			assert.NotErrorIs(t, err, ErrInsufficientResources)
		})
	}
}

func TestCheckVGPUCapacity(t *testing.T) {
	clean := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
	dirty := devices.VirtualFunction{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0", Allocated: true, ProfileType: testVFProfileType}
	tests := []struct {
		name       string
		framework  devices.VGPUFramework
		vfs        []devices.VirtualFunction
		profiles   map[string][]devices.VGPUProfileType
		claimDirty bool
		wantErr    error
	}{
		{
			name:      "profile creatable",
			framework: devices.VGPUFrameworkVendorVFIO,
			vfs:       []devices.VirtualFunction{clean},
			profiles:  testVFProfiles(clean.PCIAddress),
		},
		{
			name:       "host full",
			framework:  devices.VGPUFrameworkVendorVFIO,
			vfs:        []devices.VirtualFunction{clean, dirty},
			profiles:   map[string][]devices.VGPUProfileType{clean.PCIAddress: {}, dirty.PCIAddress: {}},
			claimDirty: true,
			wantErr:    ErrInsufficientResources,
		},
		{
			name:       "creatable types unreadable",
			framework:  devices.VGPUFrameworkVendorVFIO,
			vfs:        []devices.VirtualFunction{clean, dirty},
			profiles:   map[string][]devices.VGPUProfileType{},
			claimDirty: true,
		},
		{
			name:       "free VF unreadable",
			framework:  devices.VGPUFrameworkVendorVFIO,
			vfs:        []devices.VirtualFunction{clean, dirty},
			profiles:   map[string][]devices.VGPUProfileType{dirty.PCIAddress: {}},
			claimDirty: true,
		},
		{
			name:      "dirty unclaimed VF may hide capacity",
			framework: devices.VGPUFrameworkVendorVFIO,
			vfs:       []devices.VirtualFunction{clean, dirty},
			profiles:  map[string][]devices.VGPUProfileType{clean.PCIAddress: {}, dirty.PCIAddress: {}},
		},
		{
			name:      "mdev framework is left to the claim",
			framework: devices.VGPUFrameworkMdev,
			vfs:       []devices.VirtualFunction{clean},
			profiles:  map[string][]devices.VGPUProfileType{clean.PCIAddress: {}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &manager{
				paths: paths.New(t.TempDir()),
				discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
					return tt.framework, tt.vfs, nil
				},
				vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
					return tt.profiles, nil
				},
				quarantinedVFs: func() (map[string]struct{}, error) { return nil, nil },
			}
			if tt.claimDirty {
				owner := saveTestVGPUInstance(t, m, "owner")
				owner.GPUFramework = devices.VGPUFrameworkVendorVFIO
				owner.GPUDevicePath = "/sys/bus/pci/devices/" + dirty.PCIAddress
				require.NoError(t, m.saveMetadata(owner))
			}

			err := m.checkVGPUCapacity(testVGPUProfile)
			if tt.wantErr != nil {
				require.ErrorIs(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
		})
	}
}

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

func TestCreateInstanceRejectsFullVGPUHostBeforeResourceReservation(t *testing.T) {
	if !devices.Capabilities().SupportsVGPU {
		t.Skip("host platform does not support vGPU")
	}
	vf := devices.VirtualFunction{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"}
	validator := &recordingResourceValidator{}
	m := &manager{
		paths:             paths.New(t.TempDir()),
		resourceValidator: validator,
		discoverVGPU: func() (devices.VGPUFramework, []devices.VirtualFunction, error) {
			return devices.VGPUFrameworkVendorVFIO, []devices.VirtualFunction{vf}, nil
		},
		vendorVFIOProfiles: func([]devices.VirtualFunction) (map[string][]devices.VGPUProfileType, error) {
			return map[string][]devices.VGPUProfileType{vf.PCIAddress: {}}, nil
		},
		quarantinedVFs: func() (map[string]struct{}, error) { return nil, nil },
	}

	_, err := m.createInstance(context.Background(), CreateInstanceRequest{
		Name:  "instance",
		Image: "image",
		GPU:   &GPUConfig{Profile: testVGPUProfile},
	})

	require.ErrorIs(t, err, ErrInsufficientResources)
	assert.Zero(t, validator.reserveCalls)
}
