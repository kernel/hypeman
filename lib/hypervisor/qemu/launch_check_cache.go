package qemu

import "sync"

// launchCheckCache runs a launch-readiness probe once and keeps its result for
// the life of the process. QEMU readiness is startup state: changing the
// system binary or vhost-vsock device requires restarting Hypeman. Caching the
// first result also ensures the standard and microvm registrations, plus
// concurrent capability requests, share one bounded probe.
type launchCheckCache struct {
	probe func() error

	once   sync.Once
	result error
}

func newLaunchCheckCache(probe func() error) *launchCheckCache {
	return &launchCheckCache{probe: probe}
}

// Check returns the first probe result for the life of this cache. sync.Once
// coalesces concurrent callers and publishes result safely to every waiter.
func (c *launchCheckCache) Check() error {
	c.once.Do(func() {
		c.result = c.probe()
	})
	return c.result
}
