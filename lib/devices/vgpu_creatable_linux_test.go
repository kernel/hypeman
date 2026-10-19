//go:build linux

package devices

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testCreatablePlacements is what NVML reported on a host with two L40S GPUs
// in mixed-size mode, trimmed to three profiles. GPU 82 holds twelve vGPUs
// (six 4Q, six 2Q), so its creatable_vgpu_types lists hide 4Q although
// placements 36 and 44 are free. GPU e3 holds four 2Q and two 4Q.
func testCreatablePlacements() map[string][]creatableProfile {
	twoQ := VGPUProfileType{TypeName: "1148", Name: "NVIDIA L40S-2Q", FramebufferMB: 2048}
	fourQ := VGPUProfileType{TypeName: "1150", Name: "NVIDIA L40S-4Q", FramebufferMB: 4096}
	twelveQ := VGPUProfileType{TypeName: "1153", Name: "NVIDIA L40S-12Q", FramebufferMB: 12288}
	return map[string][]creatableProfile{
		"0000:82:00.0": {
			{twoQ, []int{36, 40, 42, 46}},
			{fourQ, []int{36, 44}},
			{twelveQ, []int{36}},
		},
		"0000:e3:00.0": {
			{twoQ, []int{18, 22, 24, 28, 30, 34, 36, 40, 42, 46}},
			{fourQ, []int{20, 24, 32, 36, 44}},
			{twelveQ, []int{24, 36}},
		},
	}
}

func (s *testVendorVFIOSysfs) useCreatablePlacements(t *testing.T) {
	t.Helper()
	s.creatableFunc = func(gpus []string) (map[string][]creatableProfile, error) {
		byGPU := make(map[string][]creatableProfile, len(gpus))
		for _, gpu := range gpus {
			profiles, ok := testCreatablePlacements()[gpu]
			require.True(t, ok, "unexpected GPU %s", gpu)
			byGPU[gpu] = profiles
		}
		return byGPU, nil
	}
}

func setNVMLTimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := nvmlTimeout
	nvmlTimeout = timeout
	t.Cleanup(func() { nvmlTimeout = previous })
}

func waitForStuckNVML(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return nvmlStuck.Load() == 0 }, 10*time.Second, 10*time.Millisecond)
}

func TestNVMLQueryFailsWithoutUsableLibrary(t *testing.T) {
	previous := nvmlLibrary
	nvmlMu.Lock()
	loaded := nvml
	nvml = nil
	nvmlMu.Unlock()
	t.Cleanup(func() {
		nvmlLibrary = previous
		nvmlMu.Lock()
		nvml = loaded
		nvmlMu.Unlock()
	})

	nvmlLibrary = "libnvidia-ml-missing.so.1"
	_, err := nvmlCreatableProfiles([]string{"0000:82:00.0"})
	require.ErrorContains(t, err, "load libnvidia-ml-missing.so.1")

	// A library without the NVML symbols, as an old driver could ship, is
	// an error rather than a panic.
	nvmlLibrary = "libc.so.6"
	_, err = nvmlCreatableProfiles([]string{"0000:82:00.0"})
	require.ErrorContains(t, err, "from libc.so.6")
	assert.Nil(t, nvml)
}

func TestVendorVFIOProfileTypesUseCreatablePlacements(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.useCreatablePlacements(t)
	var queried []string
	query := sysfs.creatableFunc
	sysfs.creatableFunc = func(gpus []string) (map[string][]creatableProfile, error) {
		queried = gpus
		return query(gpus)
	}
	const hiddenFromSysfs = "ID    : vGPU Name\n1148  : NVIDIA L40S-2Q\n"
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "ID    : vGPU Name\n")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", hiddenFromSysfs)

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Equal(t, []string{"0000:82:00.0"}, queried, "each parent GPU is queried once")
	assert.Empty(t, profilesByVF["0000:82:00.4"], "a configured VF takes no profile")
	assert.Len(t, profilesByVF["0000:82:00.5"], 3)
	assert.Contains(t, profilesByVF["0000:82:00.5"], VGPUProfileType{TypeName: "1150", Name: "NVIDIA L40S-4Q", FramebufferMB: 4096})
}

func TestVendorVFIOProfileTypesFallBackToSysfs(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	before := NVMLFallbacks()["error"]

	sysfs.creatableFunc = func([]string) (map[string][]creatableProfile, error) {
		return nil, errors.New("nvmlInit_v2: Driver Not Loaded")
	}
	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Equal(t, []VGPUProfileType{
		{TypeName: "1147", Name: "NVIDIA L40S-1Q", FramebufferMB: 1024},
		{TypeName: "1148", Name: "NVIDIA L40S-2Q", FramebufferMB: 2048},
		{TypeName: "1159", Name: "NVIDIA L40S-48Q", FramebufferMB: 49152},
	}, profilesByVF["0000:82:00.4"])
	assert.Equal(t, before+1, NVMLFallbacks()["error"])
}

func TestVendorVFIOProfileTypesSkipNVMLWhileAQueryIsStuck(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	setNVMLTimeout(t, 50*time.Millisecond)
	before := NVMLFallbacks()

	release := make(chan struct{})
	var calls atomic.Int32
	sysfs.creatableFunc = func([]string) (map[string][]creatableProfile, error) {
		calls.Add(1)
		<-release
		return nil, errors.New("driver gone")
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		waitForStuckNVML(t)
	})

	start := time.Now()
	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second)
	assert.Len(t, profilesByVF["0000:82:00.4"], 3, "falls back to the sysfs list")

	_, err = sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load(), "no second query while the first is stuck")
	after := NVMLFallbacks()
	assert.Equal(t, before["timeout"]+1, after["timeout"])
	assert.Equal(t, before["stuck"]+1, after["stuck"])

	close(release)
	waitForStuckNVML(t)
	sysfs.useCreatablePlacements(t)
	profilesByVF, err = sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Contains(t, profilesByVF["0000:82:00.4"], VGPUProfileType{TypeName: "1153", Name: "NVIDIA L40S-12Q", FramebufferMB: 12288},
		"queries NVML again once the stuck query returns")
}

func TestVendorVFIOListProfilesCountsFreePlacements(t *testing.T) {
	resetVFHealthStore(t)
	quarantineVF(t, "0000:82:00.7")

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.useCreatablePlacements(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "ID    : vGPU Name\n")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.6", "44", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.7", "45", "0", testCreatableTypes)
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "46", "0", testCreatableTypes)

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	availability, err := GetVGPUAvailability(VGPUFrameworkVendorVFIO, vfs)
	require.NoError(t, err)
	profiles, err := sysfs.listProfiles(vfs, availability.Quarantined)
	require.NoError(t, err)
	// GPU 82 has two free healthy VFs; GPU e3 has one.
	// 4Q: min(2 placements, 2 VFs) + min(5 placements, 1 VF).
	assert.Equal(t, 3, profileAvailability(profiles, "NVIDIA L40S-4Q"))
	// 2Q: min(4 placements, 2 VFs) + min(10 placements, 1 VF).
	assert.Equal(t, 3, profileAvailability(profiles, "NVIDIA L40S-2Q"))
	// 12Q: min(1 placement, 2 VFs) + min(2 placements, 1 VF).
	assert.Equal(t, 2, profileAvailability(profiles, "NVIDIA L40S-12Q"))
}

func TestVendorVFIOFallsBackToSysfsOnlyForGPUsNVMLFails(t *testing.T) {
	resetVFHealthStore(t)
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.creatableFunc = func(gpus []string) (map[string][]creatableProfile, error) {
		return map[string][]creatableProfile{"0000:82:00.0": testCreatablePlacements()["0000:82:00.0"]},
			errors.New("GPU 0000:e3:00.0: not in mixed-size vGPU mode")
	}
	const hiddenFromSysfs = "ID    : vGPU Name\n1148  : NVIDIA L40S-2Q\n"
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", hiddenFromSysfs)
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "43", "0", testCreatableTypes)
	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	before := NVMLFallbacks()["error"]

	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Contains(t, profilesByVF["0000:82:00.4"], VGPUProfileType{TypeName: "1150", Name: "NVIDIA L40S-4Q", FramebufferMB: 4096},
		"the GPU NVML answered for keeps the profile sysfs hides")
	assert.Equal(t, []VGPUProfileType{
		{TypeName: "1147", Name: "NVIDIA L40S-1Q", FramebufferMB: 1024},
		{TypeName: "1148", Name: "NVIDIA L40S-2Q", FramebufferMB: 2048},
		{TypeName: "1159", Name: "NVIDIA L40S-48Q", FramebufferMB: 49152},
	}, profilesByVF["0000:e3:00.4"], "the failed GPU uses its sysfs list")
	assert.Equal(t, before+1, NVMLFallbacks()["error"])

	profiles, err := sysfs.listProfiles(vfs, nil)
	require.NoError(t, err)
	// 2Q: min(4 placements, 1 VF) on GPU 82 plus one advertising VF on e3.
	assert.Equal(t, 2, profileAvailability(profiles, "NVIDIA L40S-2Q"))
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-4Q"))
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-48Q"))
}
