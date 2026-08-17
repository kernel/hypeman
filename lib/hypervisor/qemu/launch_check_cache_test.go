package qemu

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestLaunchCheckCacheCoalescesConcurrentProbes pins that concurrent Check
// calls share one probe: the standard and microvm registrations plus a burst
// of capability requests must not each execute `qemu --version`.
func TestLaunchCheckCacheCoalescesConcurrentProbes(t *testing.T) {
	t.Parallel()

	const callers = 16
	var probes atomic.Int32
	release := make(chan struct{})
	entered := make(chan struct{})
	cache := newLaunchCheckCache(func() error {
		probes.Add(1)
		close(entered) // panics if a duplicate probe ever starts
		<-release
		return nil
	})

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

// TestLaunchCheckCacheKeepsFirstResult pins process-lifetime semantics. Host
// readiness changes require a Hypeman restart rather than an in-process cache
// refresh that can delay a capability response or race with host mutation.
func TestLaunchCheckCacheKeepsFirstResult(t *testing.T) {
	t.Parallel()

	probeErr := errors.New("qemu prerequisites unavailable")
	result := probeErr
	var probes int
	cache := newLaunchCheckCache(func() error {
		probes++
		return result
	})

	require.ErrorIs(t, cache.Check(), probeErr)
	result = nil
	require.ErrorIs(t, cache.Check(), probeErr)
	require.Equal(t, 1, probes, "the first readiness result must remain cached until restart")
}
