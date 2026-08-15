package qemu

import (
	"sync"
	"time"
)

// launchCheckCacheTTL is how long a launch-prerequisite result stays valid.
// It is deliberately short: long enough that the standard and microvm
// registrations (and a burst of concurrent capability requests) share a
// single `qemu --version` execution, but short enough that installing QEMU or
// loading vhost_vsock flips availability promptly without a server restart.
const launchCheckCacheTTL = time.Second

// launchCheckCache coalesces and briefly caches a launch-prerequisite probe.
// The probe executes the QEMU binary, so running it once per registry entry
// per request is wasteful and, with a hung binary, dangerous: both QEMU
// registrations share one cache instance, concurrent callers wait on a
// single in-flight probe instead of spawning duplicates, and results (success
// or failure alike) expire after ttl so repaired host prerequisites become
// visible on the next read.
type launchCheckCache struct {
	probe func() error
	ttl   time.Duration
	now   func() time.Time // seam for deterministic expiry tests

	mu       sync.Mutex
	inflight chan struct{} // non-nil while a probe runs; closed on completion
	valid    bool
	result   error
	expires  time.Time
}

func newLaunchCheckCache(probe func() error, ttl time.Duration) *launchCheckCache {
	return &launchCheckCache{probe: probe, ttl: ttl, now: time.Now}
}

// Check returns the cached probe result, refreshing it when expired. Exactly
// one caller runs the probe at a time; concurrent callers needing a fresh
// result block until the in-flight probe completes and then share it rather
// than spawning duplicates. The probe runs outside the lock, so reads that
// hit a still-valid entry are never blocked by a slow probe.
func (c *launchCheckCache) Check() error {
	for {
		c.mu.Lock()
		if c.valid && c.now().Before(c.expires) {
			err := c.result
			c.mu.Unlock()
			return err
		}
		if c.inflight != nil {
			wait := c.inflight
			c.mu.Unlock()
			<-wait
			// Re-read: the finished probe populated the cache (or another
			// refresh is already underway).
			continue
		}
		done := make(chan struct{})
		c.inflight = done
		c.mu.Unlock()

		err := c.probe()

		c.mu.Lock()
		c.valid = true
		c.result = err
		c.expires = c.now().Add(c.ttl)
		c.inflight = nil
		c.mu.Unlock()
		close(done)
		return err
	}
}

// Invalidate drops any cached result so the next Check re-probes. It is the
// explicit invalidation seam for callers (and tests) that know host state
// changed and should not wait out the TTL.
func (c *launchCheckCache) Invalidate() {
	c.mu.Lock()
	c.valid = false
	c.mu.Unlock()
}
