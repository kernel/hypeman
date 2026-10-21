//go:build linux

package devices

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
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

func TestNVMLFallbackLogsOnlyWhenItChanges(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	nvmlFallbackLogged.Lock()
	nvmlFallbackLogged.err = ""
	nvmlFallbackLogged.Unlock()
	t.Cleanup(func() {
		slog.SetDefault(previous)
		nvmlFallbackLogged.Lock()
		nvmlFallbackLogged.err = ""
		nvmlFallbackLogged.Unlock()
	})
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	var queryErr error
	sysfs.creatableFunc = func(gpus []string) (map[string][]creatableProfile, error) {
		if queryErr != nil {
			return nil, queryErr
		}
		return map[string][]creatableProfile{"0000:82:00.0": testCreatablePlacements()["0000:82:00.0"]}, nil
	}
	before := NVMLFallbacks()

	equalSize := errors.Join(fmt.Errorf("GPU 0000:82:00.0: %w", errNVMLEqualSize))
	failed := errors.Join(errors.New("GPU 0000:82:00.0: nvmlDeviceGetSupportedVgpus: Unknown Error"))
	for _, err := range []error{equalSize, equalSize, failed, failed, nil, nil, equalSize} {
		queryErr = err
		_, err := sysfs.profileTypes(vfs)
		require.NoError(t, err)
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	require.Len(t, lines, 4, logs.String())
	assert.Contains(t, lines[0], "reason=equal_size")
	assert.Contains(t, lines[1], "reason=error")
	assert.Contains(t, lines[2], "available for every GPU again")
	assert.Contains(t, lines[3], "reason=equal_size")
	after := NVMLFallbacks()
	assert.Equal(t, before["equal_size"]+3, after["equal_size"], "every fallback is counted")
	assert.Equal(t, before["error"]+2, after["error"])
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

func TestNVMLQueriesStartOneAtATime(t *testing.T) {
	setNVMLTimeout(t, 100*time.Millisecond)
	release := make(chan struct{})
	var entered atomic.Int32
	hung := func() (int, error) {
		entered.Add(1)
		<-release
		return 0, nil
	}
	t.Cleanup(func() {
		close(release)
		waitForStuckNVML(t)
	})

	errs := make(chan error, 5)
	for range 5 {
		go func() {
			_, err := runBounded(hung)
			errs <- err
		}()
	}
	var timeouts, stuck int
	for range 5 {
		err := <-errs
		switch {
		case errors.Is(err, errNVMLTimeout):
			timeouts++
		case errors.Is(err, errNVMLStuck):
			stuck++
		}
	}
	assert.Equal(t, 1, timeouts)
	assert.Equal(t, 4, stuck)
	assert.Equal(t, int32(1), entered.Load(), "callers behind a hung query never enter the driver")
}

func TestNVMLQueriesWaitForEachOther(t *testing.T) {
	var active, overlaps atomic.Int32
	query := func() (int, error) {
		if active.Add(1) > 1 {
			overlaps.Add(1)
		}
		time.Sleep(5 * time.Millisecond)
		active.Add(-1)
		return 1, nil
	}

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			value, err := runBounded(query)
			assert.NoError(t, err)
			assert.Equal(t, 1, value)
		})
	}
	wg.Wait()
	assert.Zero(t, overlaps.Load())
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
			errors.Join(fmt.Errorf("GPU 0000:e3:00.0: %w", errNVMLEqualSize))
	}
	const hiddenFromSysfs = "ID    : vGPU Name\n1148  : NVIDIA L40S-2Q\n"
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", hiddenFromSysfs)
	sysfs.addVF(t, "0000:e3:00.0", "0000:e3:00.4", "43", "0", testCreatableTypes)
	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	before := NVMLFallbacks()

	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Contains(t, profilesByVF["0000:82:00.4"], VGPUProfileType{TypeName: "1150", Name: "NVIDIA L40S-4Q", FramebufferMB: 4096},
		"the GPU NVML answered for keeps the profile sysfs hides")
	assert.Equal(t, []VGPUProfileType{
		{TypeName: "1147", Name: "NVIDIA L40S-1Q", FramebufferMB: 1024},
		{TypeName: "1148", Name: "NVIDIA L40S-2Q", FramebufferMB: 2048},
		{TypeName: "1159", Name: "NVIDIA L40S-48Q", FramebufferMB: 49152},
	}, profilesByVF["0000:e3:00.4"], "the failed GPU uses its sysfs list")
	after := NVMLFallbacks()
	assert.Equal(t, before["equal_size"]+1, after["equal_size"])
	assert.Equal(t, before["error"], after["error"])

	profiles, err := sysfs.listProfiles(vfs, nil)
	require.NoError(t, err)
	// 2Q: min(4 placements, 1 VF) on GPU 82 plus one advertising VF on e3.
	assert.Equal(t, 2, profileAvailability(profiles, "NVIDIA L40S-2Q"))
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-4Q"))
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-48Q"))
}

func TestNVMLFallbackReasons(t *testing.T) {
	equalSize := fmt.Errorf("GPU 0000:e3:00.0: %w", errNVMLEqualSize)
	failed := errors.New("GPU 0000:82:00.0: nvmlDeviceGetSupportedVgpus: Unknown Error")
	for _, tc := range []struct {
		name   string
		err    error
		reason string
	}{
		{"every failed GPU is in equal-size mode", errors.Join(equalSize, equalSize), "equal_size"},
		{"one failed GPU is not in equal-size mode", errors.Join(equalSize, failed), "error"},
		{"NVML cannot load", errors.New("load libnvidia-ml.so.1: not found"), "error"},
		{"timed out", errNVMLTimeout, "timeout"},
		{"stuck", errNVMLStuck, "stuck"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := NVMLFallbacks()
			recordNVMLFallback(fmt.Errorf("query creatable vGPU placements: %w", tc.err))
			after := NVMLFallbacks()
			for reason := range after {
				want := before[reason]
				if reason == tc.reason {
					want++
				}
				assert.Equal(t, want, after[reason], reason)
			}
		})
	}
}
