package qemu

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// newTestCache builds a private cache instance around the given probe with a
// manually advanced clock. Tests never touch the package's shared
// launchPrereqCache, so parallel tests (and the real registrations) are
// unaffected.
func newTestCache(probe func() error, ttl time.Duration) (*launchCheckCache, *time.Time) {
	c := newLaunchCheckCache(probe, ttl)
	now := time.Unix(1700000000, 0)
	c.now = func() time.Time { return now }
	return c, &now
}

// TestLaunchCheckCacheCoalescesConcurrentProbes pins that concurrent Check
// calls on an empty cache share a single in-flight probe: the standard and
// microvm registrations plus a burst of capability requests must not each
// execute `qemu --version`.
func TestLaunchCheckCacheCoalescesConcurrentProbes(t *testing.T) {
	t.Parallel()

	const callers = 16
	var probes atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})
	cache, _ := newTestCache(func() error {
		probes.Add(1)
		close(entered) // panics if a duplicate probe ever starts
		<-release
		return nil
	}, time.Minute)

	var wg sync.WaitGroup
	results := make([]error, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = cache.Check()
		}()
	}
	<-entered
	close(release)
	wg.Wait()

	require.Equal(t, int32(1), probes.Load(), "concurrent callers must coalesce onto one probe")
	for i := range callers {
		require.NoError(t, results[i], "every waiter must receive the shared probe result")
	}
}

// TestLaunchCheckCacheExpiry pins TTL semantics: results (successes and
// failures alike) are served from cache within the TTL and re-probed after it
// elapses, so a repaired host prerequisite becomes visible without a restart.
func TestLaunchCheckCacheExpiry(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("qemu binary missing")
	var result error = probeErr
	var probes int
	cache, now := newTestCache(func() error {
		probes++
		return result
	}, time.Second)

	require.ErrorIs(t, cache.Check(), probeErr)
	require.Equal(t, 1, probes)

	// Within the TTL the cached failure is served without re-probing.
	*now = now.Add(500 * time.Millisecond)
	require.ErrorIs(t, cache.Check(), probeErr)
	require.Equal(t, 1, probes, "a fresh result must be served from cache")

	// After the TTL the probe runs again and a repaired prerequisite
	// (installed binary) is reported.
	result = nil
	*now = now.Add(time.Second)
	require.NoError(t, cache.Check())
	require.Equal(t, 2, probes, "an expired result must be re-probed")

	// A subsequent failure is likewise picked up after expiry.
	result = probeErr
	*now = now.Add(2 * time.Second)
	require.ErrorIs(t, cache.Check(), probeErr)
	require.Equal(t, 3, probes)
}

// TestLaunchCheckCacheInvalidate pins the explicit invalidation seam: callers
// that know host state changed can force the next Check to re-probe without
// waiting out the TTL.
func TestLaunchCheckCacheInvalidate(t *testing.T) {
	t.Parallel()

	var probes int
	cache, _ := newTestCache(func() error {
		probes++
		return nil
	}, time.Hour)

	require.NoError(t, cache.Check())
	require.NoError(t, cache.Check())
	require.Equal(t, 1, probes, "an unexpired result must be served from cache")

	cache.Invalidate()
	require.NoError(t, cache.Check())
	require.Equal(t, 2, probes, "Invalidate must force the next Check to re-probe")
}
