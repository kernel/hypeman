//go:build linux

package devices

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func testCapacityVFs() []VirtualFunction {
	return []VirtualFunction{
		{PCIAddress: "0000:82:00.4", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:82:00.5", ParentGPU: "0000:82:00.0"},
		{PCIAddress: "0000:e3:00.4", ParentGPU: "0000:e3:00.0"},
	}
}

func TestCapacityCacheReadsDoNotQueryTheDriver(t *testing.T) {
	var loads atomic.Int32
	cache := newCapacityCache(func() (creatableSnapshot, error) {
		loads.Add(1)
		return creatableSnapshot{byGPU: testCreatablePlacements()}, nil
	})
	vfs := testCapacityVFs()

	profiles, err := cache.profiles(vfs, nil)
	require.NoError(t, err)
	// 4Q: min(2 placements, 2 VFs) on GPU 82 + min(5 placements, 1 VF) on e3.
	assert.Equal(t, 3, profileAvailability(profiles, "NVIDIA L40S-4Q"))

	vfs[0].Allocated = true
	profiles, err = cache.profiles(vfs, map[string]struct{}{"0000:e3:00.4": {}})
	require.NoError(t, err)
	assert.Equal(t, 1, profileAvailability(profiles, "NVIDIA L40S-4Q"),
		"a claimed or quarantined VF counts before the placements are reloaded")
	assert.Equal(t, int32(1), loads.Load(), "only the first read loads")
}

func TestCapacityCacheBackgroundRefreshesCoalesce(t *testing.T) {
	started := make(chan struct{}, 10)
	release := make(chan struct{})
	var loads atomic.Int32
	cache := newCapacityCache(func() (creatableSnapshot, error) {
		n := loads.Add(1)
		started <- struct{}{}
		<-release
		if n == 1 {
			return creatableSnapshot{}, nil
		}
		return creatableSnapshot{byGPU: testCreatablePlacements()}, nil
	})

	cache.refreshAsync()
	<-started
	// A change made while a load runs needs one more load, however many
	// refreshes it requests.
	for range 5 {
		cache.refreshAsync()
	}
	close(release)
	require.Eventually(t, func() bool { return loads.Load() == 2 }, 5*time.Second, time.Millisecond)
	require.Eventually(t, func() bool {
		profiles, err := cache.profiles(testCapacityVFs(), nil)
		return err == nil && profileAvailability(profiles, "NVIDIA L40S-4Q") == 3
	}, 5*time.Second, time.Millisecond, "the newer load replaces the older one")
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, int32(2), loads.Load())
}

func TestVendorVFIOConfigureAndDestroyRefreshCapacity(t *testing.T) {
	resetVFHealthStore(t)
	sysfs := newTestVendorVFIOSysfs(t)
	sysfs.useCreatablePlacements(t)
	sysfs.addVF(t, "0000:82:00.0", "0000:82:00.4", "42", "0", testCreatableTypes)
	previousSysfs, previousCache := hostVendorVFIO, vendorVFIOCapacity
	hostVendorVFIO = sysfs.vendorVFIOSysfs
	var loads atomic.Int32
	vendorVFIOCapacity = newCapacityCache(func() (creatableSnapshot, error) {
		snapshot, err := hostVendorVFIO.loadCapacity()
		loads.Add(1)
		return snapshot, err
	})
	t.Cleanup(func() { hostVendorVFIO, vendorVFIOCapacity = previousSysfs, previousCache })

	require.NoError(t, ConfigureVGPU(context.Background(), "0000:82:00.4", "1150"))
	require.Eventually(t, func() bool { return loads.Load() == 1 }, 5*time.Second, time.Millisecond)
	require.NoError(t, DestroyVGPU(context.Background(), VGPUAssignment{
		Framework:  VGPUFrameworkVendorVFIO,
		DevicePath: GetDeviceSysfsPath("0000:82:00.4"),
	}))
	require.Eventually(t, func() bool { return loads.Load() == 2 }, 5*time.Second, time.Millisecond)
}
