package instances

import (
	"os"
	"strconv"
	"testing"
)

// heavyIOSlots limits how many VM/snapshot-heavy integration tests run
// concurrently. Go's default -parallel is GOMAXPROCS, which on big CI runners
// lets hundreds of VM-booting tests run at once. Under that contention guests
// boot/restore too slowly and their in-guest agents never connect, producing
// flaky timeouts. Light unit tests are left fully parallel; only tests that
// boot VMs and do snapshot/fork/restore/UFFD/warm-fork/compression work
// acquire a slot via acquireHeavyIO.
//
// The slot count is controlled by HYPEMAN_TEST_HEAVY_IO_PARALLELISM (default
// 4). A value < 1 disables throttling entirely (unlimited concurrency).
var heavyIOSlots = newHeavyIOSlots()

const defaultHeavyIOParallelism = 4

func newHeavyIOSlots() chan struct{} {
	limit := defaultHeavyIOParallelism
	if v := os.Getenv("HYPEMAN_TEST_HEAVY_IO_PARALLELISM"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	if limit < 1 {
		// Unlimited: a nil channel means acquireHeavyIO is a no-op.
		return nil
	}
	return make(chan struct{}, limit)
}

// acquireHeavyIO blocks until a heavy-IO slot is available, then registers a
// t.Cleanup to release it. It is safe to call after t.Parallel(): the slot is
// held for the remainder of the test (including its cleanups) and released
// exactly once. When throttling is disabled it returns immediately.
func acquireHeavyIO(t *testing.T) {
	t.Helper()
	if heavyIOSlots == nil {
		return
	}
	heavyIOSlots <- struct{}{}
	t.Cleanup(func() { <-heavyIOSlots })
}
