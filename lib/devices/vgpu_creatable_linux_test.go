//go:build linux

package devices

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixture is nvidia-smi vgpu -c -v from a host with two L40S GPUs in
// mixed-size mode. GPU 82 holds twelve vGPUs (six 4Q, six 2Q), so its
// creatable_vgpu_types lists hide 4Q although placements 36 and 44 are free.
// GPU e3 holds four 2Q and two 4Q.
func readCreatableFixture(t *testing.T) []byte {
	t.Helper()
	data, err := os.ReadFile("testdata/nvidia-smi-vgpu-creatable-l40s.txt")
	require.NoError(t, err)
	return data
}

func creatableProfileByName(profiles []creatableProfile, name string) (creatableProfile, bool) {
	for _, profile := range profiles {
		if profile.Name == name {
			return profile, true
		}
	}
	return creatableProfile{}, false
}

func TestParseCreatableProfiles(t *testing.T) {
	byGPU, err := parseCreatableProfiles(string(readCreatableFixture(t)))
	require.NoError(t, err)
	require.Len(t, byGPU, 2)
	assert.Len(t, byGPU["0000:82:00.0"], 21)

	large, ok := creatableProfileByName(byGPU["0000:82:00.0"], "NVIDIA L40S-4Q")
	require.True(t, ok, "4Q has free placements on GPU 82")
	assert.Equal(t, VGPUProfileType{TypeName: "1150", Name: "NVIDIA L40S-4Q", FramebufferMB: 4096}, large.VGPUProfileType)
	assert.Equal(t, []int{36, 44}, large.placements)

	small, ok := creatableProfileByName(byGPU["0000:e3:00.0"], "NVIDIA L40S-2Q")
	require.True(t, ok)
	assert.Equal(t, "1148", small.TypeName)
	assert.Equal(t, []int{18, 22, 24, 28, 30, 34, 36, 40, 42, 46}, small.placements)
}

func TestParseCreatableProfilesFullGPU(t *testing.T) {
	// A full GPU is listed with no types. The indented "GPU Instance Profile
	// ID" field is not a GPU header.
	output := `GPU 00000000:82:00.0
No vGPUs found on this device 

GPU 00000000:E3:00.0
    vGPU Type ID                          : 0x47c
        Name                              : NVIDIA L40S-2Q
        GPU Instance Profile ID           : N/A
        Placement Size                    : 2
        Placement IDs                     : 18 22 
`
	byGPU, err := parseCreatableProfiles(output)
	require.NoError(t, err)
	require.Contains(t, byGPU, "0000:82:00.0")
	assert.Empty(t, byGPU["0000:82:00.0"])
	require.Len(t, byGPU["0000:e3:00.0"], 1)
	assert.Equal(t, []int{18, 22}, byGPU["0000:e3:00.0"][0].placements)
}

func TestParseCreatableProfilesRejectsUnexpectedOutput(t *testing.T) {
	for name, output := range map[string]string{
		"driver unreachable":     "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.\n",
		"placements unavailable": "GPU 00000000:82:00.0\n    vGPU Type ID : 0x47c\n        Name : NVIDIA L40S-2Q\n        Placement IDs : N/A\n",
		"no placements":          "GPU 00000000:82:00.0\n    vGPU Type ID : 0x47c\n        Name : NVIDIA L40S-2Q\n",
		"type before GPU":        "    vGPU Type ID : 0x47c\n        Name : NVIDIA L40S-2Q\n        Placement IDs : 18\n",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parseCreatableProfiles(output)
			require.Error(t, err)
		})
	}
}

func TestRunNvidiaSMIStopsAtDeadline(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\nexec sleep 30\n"), 0755))
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := runNvidiaSMI(ctx, "vgpu", "-c", "-v")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second)
}

func setNvidiaSMITimeout(t *testing.T, timeout time.Duration) {
	t.Helper()
	previous := nvidiaSMITimeout
	nvidiaSMITimeout = timeout
	t.Cleanup(func() { nvidiaSMITimeout = previous })
}

func waitForStuckNvidiaSMI(t *testing.T) {
	t.Helper()
	require.Eventually(t, func() bool { return nvidiaSMIStuck.Load() == 0 }, 10*time.Second, 10*time.Millisecond)
}

func TestCreatableQueryReturnsAtDeadlineWhenNvidiaSMIOutlivesKill(t *testing.T) {
	dir := t.TempDir()
	// The shell is killed at the deadline, but its child keeps stdout open,
	// so the run does not return until the child exits.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte("#!/bin/sh\nsleep 2 &\nwait\n"), 0755))
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	setNvidiaSMITimeout(t, 100*time.Millisecond)
	t.Cleanup(func() { waitForStuckNvidiaSMI(t) })

	start := time.Now()
	_, err := runBounded(runNvidiaSMI, "vgpu", "-c", "-v")
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), time.Second)
}

func TestVendorVFIOProfileTypesSkipNvidiaSMIWhileARunIsStuck(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	setNvidiaSMITimeout(t, 50*time.Millisecond)
	before := NvidiaSMIFallbacks()

	release := make(chan struct{})
	var calls atomic.Int32
	sysfs.nvidiaSMIFunc = func(context.Context, ...string) ([]byte, error) {
		calls.Add(1)
		<-release
		return nil, errors.New("signal: killed")
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		waitForStuckNvidiaSMI(t)
	})

	start := time.Now()
	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Less(t, time.Since(start), time.Second)
	assert.Len(t, profilesByVF["0000:82:00.4"], 3, "falls back to the sysfs list")

	_, err = sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Equal(t, int32(1), calls.Load(), "no second run while the first is stuck")
	after := NvidiaSMIFallbacks()
	assert.Equal(t, before["timeout"]+1, after["timeout"])
	assert.Equal(t, before["stuck"]+1, after["stuck"])

	close(release)
	waitForStuckNvidiaSMI(t)
	sysfs.useCreatableFixture(t)
	profilesByVF, err = sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Len(t, profilesByVF["0000:82:00.4"], 21, "queries nvidia-smi again once the stuck run returns")
}

func (s *testVendorVFIOSysfs) useCreatableFixture(t *testing.T) {
	t.Helper()
	data := readCreatableFixture(t)
	s.nvidiaSMIFunc = func(_ context.Context, args ...string) ([]byte, error) {
		assert.Equal(t, []string{"vgpu", "-c", "-v"}, args)
		return data, nil
	}
}

func TestVendorVFIOProfileTypesUseCreatablePlacements(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.useCreatableFixture(t)
	const hiddenFromSysfs = "ID    : vGPU Name\n1148  : NVIDIA L40S-2Q\n"
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "1148", "ID    : vGPU Name\n")
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.5", "43", "0", hiddenFromSysfs)

	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Empty(t, profilesByVF["0000:82:00.4"], "a configured VF takes no profile")
	assert.Len(t, profilesByVF["0000:82:00.5"], 21)
	assert.Contains(t, profilesByVF["0000:82:00.5"], VGPUProfileType{TypeName: "1150", Name: "NVIDIA L40S-4Q", FramebufferMB: 4096})
}

func TestVendorVFIOProfileTypesFallBackToSysfs(t *testing.T) {
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	vfs, err := sysfs.discoverVFs()
	require.NoError(t, err)
	sysfsProfiles := []VGPUProfileType{
		{TypeName: "1147", Name: "NVIDIA L40S-1Q", FramebufferMB: 1024},
		{TypeName: "1148", Name: "NVIDIA L40S-2Q", FramebufferMB: 2048},
		{TypeName: "1159", Name: "NVIDIA L40S-48Q", FramebufferMB: 49152},
	}

	before := NvidiaSMIFallbacks()["error"]

	sysfs.nvidiaSMIFunc = func(context.Context, ...string) ([]byte, error) {
		return nil, errors.New("exit status 9")
	}
	profilesByVF, err := sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Equal(t, sysfsProfiles, profilesByVF["0000:82:00.4"])

	// A GPU missing from the output is unknown, not full.
	sysfs.nvidiaSMIFunc = func(context.Context, ...string) ([]byte, error) {
		return []byte("GPU 00000000:E3:00.0\nNo vGPUs found on this device\n"), nil
	}
	profilesByVF, err = sysfs.profileTypes(vfs)
	require.NoError(t, err)
	assert.Equal(t, sysfsProfiles, profilesByVF["0000:82:00.4"])
	assert.Equal(t, before+2, NvidiaSMIFallbacks()["error"])
}

func TestVendorVFIOListProfilesCountsFreePlacements(t *testing.T) {
	resetVFHealthStore(t)
	quarantineVF(t, "0000:82:00.7")

	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.useCreatableFixture(t)
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
