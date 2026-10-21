//go:build linux

package devices

import (
	"fmt"
	"sync"
	"sync/atomic"
)

// vendorVFIOCapacity holds the driver's placement data for vendor VFIO
// profile counts, so /resources reads memory rather than querying NVML. It
// is loaded on first use or by the vGPU reconciler at startup, refreshed in
// the background after every vendor VFIO configure or reset, and refreshed
// again on every reconcile pass. Counts are recomputed on each read against
// the caller's VFs and quarantine, so a claimed or released VF counts at
// once; placements, which only the driver can recompute for mixed sizes,
// catch up when the background refresh lands.
var vendorVFIOCapacity = newCapacityCache(func() (creatableSnapshot, error) {
	return hostVendorVFIO.loadCapacity()
})

type capacityCache struct {
	load func() (creatableSnapshot, error)

	// refreshMu serializes loads, so an older snapshot never replaces a
	// newer one.
	refreshMu sync.Mutex
	// queued is set while a background refresh waits to start its load. A
	// change made before the load starts is seen by it, so later requests
	// join it.
	queued atomic.Bool

	mu       sync.RWMutex
	loaded   bool
	snapshot creatableSnapshot
	err      error
}

func newCapacityCache(load func() (creatableSnapshot, error)) *capacityCache {
	return &capacityCache{load: load}
}

func (s vendorVFIOSysfs) loadCapacity() (creatableSnapshot, error) {
	vfs, err := s.discoverVFs()
	if err != nil {
		return creatableSnapshot{}, fmt.Errorf("discover vendor VFIO VFs: %w", err)
	}
	return s.creatableSnapshot(vfs)
}

// profiles returns the profile counts for vfs from the cached placements,
// loading them first if nothing has been loaded yet.
func (c *capacityCache) profiles(vfs []VirtualFunction, quarantined map[string]struct{}) ([]GPUProfile, error) {
	c.mu.RLock()
	loaded := c.loaded
	c.mu.RUnlock()
	if !loaded {
		c.refresh()
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.err != nil {
		return nil, c.err
	}
	return c.snapshot.profiles(vfs, quarantined), nil
}

func (c *capacityCache) refresh() {
	c.refreshMu.Lock()
	defer c.refreshMu.Unlock()
	c.loadLocked()
}

// refreshAsync reloads the placements in the background.
func (c *capacityCache) refreshAsync() {
	if !c.queued.CompareAndSwap(false, true) {
		return
	}
	go func() {
		c.refreshMu.Lock()
		defer c.refreshMu.Unlock()
		c.queued.Store(false)
		c.loadLocked()
	}()
}

func (c *capacityCache) loadLocked() {
	snapshot, err := c.load()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.snapshot, c.err, c.loaded = snapshot, err, true
}

// RefreshVGPUCapacity reloads the vendor VFIO placements /resources counts
// from. It does nothing on hosts without vendor VFIO vGPUs.
func RefreshVGPUCapacity() {
	if framework, _, err := DiscoverVGPU(); err == nil && framework == VGPUFrameworkVendorVFIO {
		vendorVFIOCapacity.refresh()
	}
}
