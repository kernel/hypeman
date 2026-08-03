package builds

import (
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBuildQueue_EnqueueStartsImmediately(t *testing.T) {
	queue := NewBuildQueue(2)

	started := make(chan string, 2)
	done := make(chan struct{})

	// Enqueue first build - should start immediately
	pos := queue.Enqueue("build-1", CreateBuildRequest{}, func() {
		started <- "build-1"
		<-done // Wait for signal
	})

	assert.Equal(t, 0, pos, "first build should start immediately (position 0)")

	// Wait for it to start
	select {
	case id := <-started:
		assert.Equal(t, "build-1", id)
	case <-time.After(time.Second):
		t.Fatal("build-1 did not start")
	}

	close(done)
}

func TestBuildQueue_QueueWhenAtCapacity(t *testing.T) {
	queue := NewBuildQueue(1) // Max 1 concurrent

	var wg sync.WaitGroup
	done := make(chan struct{})

	// Start first build
	wg.Add(1)
	pos1 := queue.Enqueue("build-1", CreateBuildRequest{}, func() {
		wg.Done()
		<-done // Block
	})
	assert.Equal(t, 0, pos1)

	// Wait for first to actually start
	wg.Wait()

	// Second build should be queued
	pos2 := queue.Enqueue("build-2", CreateBuildRequest{}, func() {})
	assert.Equal(t, 1, pos2, "second build should be queued at position 1")

	// Third build should be queued at position 2
	pos3 := queue.Enqueue("build-3", CreateBuildRequest{}, func() {})
	assert.Equal(t, 2, pos3, "third build should be queued at position 2")

	close(done)
}

func TestBuildQueue_DeduplicationActive(t *testing.T) {
	queue := NewBuildQueue(2)
	done := make(chan struct{})

	// Start a build
	queue.Enqueue("build-1", CreateBuildRequest{}, func() {
		<-done
	})

	// Wait for it to become active
	time.Sleep(10 * time.Millisecond)

	// Try to enqueue the same build again - should return position 0 (active)
	pos := queue.Enqueue("build-1", CreateBuildRequest{}, func() {})
	assert.Equal(t, 0, pos, "re-enqueueing active build should return position 0")

	close(done)
}

func TestBuildQueue_DeduplicationPending(t *testing.T) {
	queue := NewBuildQueue(1)
	done := make(chan struct{})

	// Fill the queue
	queue.Enqueue("build-1", CreateBuildRequest{}, func() {
		<-done
	})

	// Add a second build to pending
	pos1 := queue.Enqueue("build-2", CreateBuildRequest{}, func() {})
	assert.Equal(t, 1, pos1)

	// Try to enqueue build-2 again - should return same position
	pos2 := queue.Enqueue("build-2", CreateBuildRequest{}, func() {})
	assert.Equal(t, 1, pos2, "re-enqueueing pending build should return same position")

	close(done)
}

func TestBuildQueue_Cancel(t *testing.T) {
	queue := NewBuildQueue(1)
	done := make(chan struct{})

	// Fill the queue
	queue.Enqueue("build-1", CreateBuildRequest{}, func() {
		<-done
	})

	// Add to pending
	queue.Enqueue("build-2", CreateBuildRequest{}, func() {})
	queue.Enqueue("build-3", CreateBuildRequest{}, func() {})

	// Cancel build-2
	cancelled := queue.Cancel("build-2")
	require.True(t, cancelled, "should be able to cancel pending build")

	// Verify build-3 moved up
	pos := queue.GetPosition("build-3")
	require.NotNil(t, pos)
	assert.Equal(t, 1, *pos, "build-3 should move to position 1")

	// Can't cancel active build
	cancelled = queue.Cancel("build-1")
	assert.False(t, cancelled, "should not be able to cancel active build")

	close(done)
}

func TestBuildQueue_GetPosition(t *testing.T) {
	queue := NewBuildQueue(1)
	done := make(chan struct{})

	queue.Enqueue("build-1", CreateBuildRequest{}, func() {
		<-done
	})
	queue.Enqueue("build-2", CreateBuildRequest{}, func() {})
	queue.Enqueue("build-3", CreateBuildRequest{}, func() {})

	// Active build has no position (returns nil)
	pos1 := queue.GetPosition("build-1")
	assert.Nil(t, pos1, "active build should have no position")

	// Pending builds have positions
	pos2 := queue.GetPosition("build-2")
	require.NotNil(t, pos2)
	assert.Equal(t, 1, *pos2)

	pos3 := queue.GetPosition("build-3")
	require.NotNil(t, pos3)
	assert.Equal(t, 2, *pos3)

	// Non-existent build has no position
	pos4 := queue.GetPosition("build-4")
	assert.Nil(t, pos4)

	close(done)
}

func TestBuildQueue_AutoStartNextOnComplete(t *testing.T) {
	queue := NewBuildQueue(1)

	started := make(chan string, 3)
	var mu sync.Mutex
	completionOrder := []string{}

	// Add builds
	queue.Enqueue("build-1", CreateBuildRequest{}, func() {
		started <- "build-1"
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		completionOrder = append(completionOrder, "build-1")
		mu.Unlock()
	})
	queue.Enqueue("build-2", CreateBuildRequest{}, func() {
		started <- "build-2"
		time.Sleep(10 * time.Millisecond)
		mu.Lock()
		completionOrder = append(completionOrder, "build-2")
		mu.Unlock()
	})

	// Wait for both to complete
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("builds did not complete in time")
		}
	}

	// Give time for completion
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"build-1", "build-2"}, completionOrder)
}

func TestBuildQueue_Counts(t *testing.T) {
	queue := NewBuildQueue(2)

	assert.Equal(t, 0, queue.ActiveCount())
	assert.Equal(t, 0, queue.PendingCount())
	assert.Equal(t, 0, queue.QueueLength())

	done := make(chan struct{})
	queue.Enqueue("build-1", CreateBuildRequest{}, func() { <-done })
	queue.Enqueue("build-2", CreateBuildRequest{}, func() { <-done })

	// Wait for them to start
	time.Sleep(10 * time.Millisecond)

	assert.Equal(t, 2, queue.ActiveCount())
	assert.Equal(t, 0, queue.PendingCount())
	assert.Equal(t, 2, queue.QueueLength())

	// Add a pending one
	queue.Enqueue("build-3", CreateBuildRequest{}, func() {})

	assert.Equal(t, 2, queue.ActiveCount())
	assert.Equal(t, 1, queue.PendingCount())
	assert.Equal(t, 3, queue.QueueLength())

	close(done)
}

func TestBuildQueue_SerialKeySerializesSameKey(t *testing.T) {
	queue := NewBuildQueue(2)

	release := make(chan struct{})
	started := make(chan string, 3)
	startFn := func(id string) func() {
		return func() {
			started <- id
			<-release
		}
	}

	// Two builds with the same serial key: only the first starts even though
	// a concurrency slot is free.
	pos1 := queue.EnqueueSerial("build-1", CreateBuildRequest{}, "builder-a", startFn("build-1"))
	pos2 := queue.EnqueueSerial("build-2", CreateBuildRequest{}, "builder-a", startFn("build-2"))

	assert.Equal(t, 0, pos1)
	assert.Equal(t, 1, pos2)

	select {
	case id := <-started:
		assert.Equal(t, "build-1", id)
	case <-time.After(2 * time.Second):
		t.Fatal("first build did not start")
	}
	assert.Equal(t, 1, queue.ActiveCount(), "same-key build stays pending and does not occupy a slot")
	assert.Equal(t, 1, queue.PendingCount())

	// A build with a different key starts immediately in the free slot.
	pos3 := queue.EnqueueSerial("build-3", CreateBuildRequest{}, "builder-b", startFn("build-3"))
	assert.Equal(t, 0, pos3)
	select {
	case id := <-started:
		assert.Equal(t, "build-3", id)
	case <-time.After(2 * time.Second):
		t.Fatal("different-key build did not start in the free slot")
	}

	close(release)

	// Once the first build completes, the serialized build starts.
	select {
	case id := <-started:
		assert.Equal(t, "build-2", id)
	case <-time.After(2 * time.Second):
		t.Fatal("serialized build did not start after same-key build completed")
	}
}

func TestBuildQueue_SerialKeySkipsBlockedPending(t *testing.T) {
	queue := NewBuildQueue(2)

	// Each build gets its own release channel so completing one build can
	// only unblock its own goroutine.
	release := make(map[string]chan struct{})
	for _, id := range []string{"build-1", "build-2", "build-3", "build-4"} {
		release[id] = make(chan struct{})
	}
	started := make(chan string, 4)
	startFn := func(id string) func() {
		return func() {
			started <- id
			<-release[id]
		}
	}

	queue.EnqueueSerial("build-1", CreateBuildRequest{}, "builder-a", startFn("build-1"))
	queue.EnqueueSerial("build-2", CreateBuildRequest{}, "builder-b", startFn("build-2"))
	pos3 := queue.EnqueueSerial("build-3", CreateBuildRequest{}, "builder-a", startFn("build-3"))
	pos4 := queue.EnqueueSerial("build-4", CreateBuildRequest{}, "builder-c", startFn("build-4"))
	assert.Equal(t, 1, pos3)
	assert.Equal(t, 1, pos4, "queue position skips blocked pending builds")

	pendingPos4 := queue.GetPosition("build-4")
	require.NotNil(t, pendingPos4)
	assert.Equal(t, 1, *pendingPos4)

	<-started // build-1
	<-started // build-2

	// build-2 completes: build-3 is blocked (builder-a held by build-1), so
	// build-4 starts instead of letting the slot sit idle.
	close(release["build-2"])
	select {
	case id := <-started:
		assert.Equal(t, "build-4", id, "blocked pending build is skipped for a later startable one")
	case <-time.After(2 * time.Second):
		t.Fatal("no pending build started after a slot freed")
	}
	assert.Nil(t, queue.GetPosition("build-4"), "build-4 is running after skip-ahead")
	pendingPos3 := queue.GetPosition("build-3")
	require.NotNil(t, pendingPos3)
	assert.Equal(t, 1, *pendingPos3, "blocked build stays at front once later build starts")

	// build-1 completes: build-3's key is now free and it starts.
	close(release["build-1"])
	select {
	case id := <-started:
		assert.Equal(t, "build-3", id)
	case <-time.After(2 * time.Second):
		t.Fatal("blocked build did not start once its serial key freed")
	}
	close(release["build-3"])
	close(release["build-4"])
}

// TestBuildQueue_ReleaseSerialKeyStartsSameKeyPending verifies that releasing
// a serial key lets a same-key pending build start while the active build
// continues running (e.g. waiting for image conversion after the cache
// volume is detached).
func TestBuildQueue_ReleaseSerialKeyStartsSameKeyPending(t *testing.T) {
	queue := NewBuildQueue(2)

	release := make(chan struct{})
	started := make(chan string, 2)
	startFn := func(id string) func() {
		return func() {
			started <- id
			<-release
		}
	}

	queue.EnqueueSerial("build-1", CreateBuildRequest{}, "builder-a", startFn("build-1"))
	queue.EnqueueSerial("build-2", CreateBuildRequest{}, "builder-a", startFn("build-2"))

	select {
	case id := <-started:
		assert.Equal(t, "build-1", id)
	case <-time.After(2 * time.Second):
		t.Fatal("first build did not start")
	}
	assert.Equal(t, 1, queue.PendingCount())

	// Releasing the key starts the pending same-key build while build-1 is
	// still active.
	queue.ReleaseSerialKey("build-1")
	select {
	case id := <-started:
		assert.Equal(t, "build-2", id)
	case <-time.After(2 * time.Second):
		t.Fatal("pending same-key build did not start after serial key release")
	}
	assert.Equal(t, 2, queue.ActiveCount())

	close(release)
}

func TestBuildQueue_SerialKeyIntrospection(t *testing.T) {
	queue := NewBuildQueue(1)

	release := make(chan struct{})
	startFn := func() { <-release }

	queue.EnqueueSerial("build-1", CreateBuildRequest{}, "builder-a", startFn)
	queue.EnqueueSerial("build-2", CreateBuildRequest{}, "builder-a", startFn)
	queue.EnqueueSerial("build-3", CreateBuildRequest{}, "builder-b", startFn)

	active := queue.ActiveBuildForSerialKey("builder-a")
	require.NotNil(t, active)
	assert.Equal(t, "build-1", *active)
	assert.Nil(t, queue.ActiveBuildForSerialKey("builder-b"))

	pending := queue.PendingBuildsForSerialKey("builder-a")
	assert.Equal(t, []string{"build-2"}, pending)
	assert.NotNil(t, queue.PendingBuildsForSerialKey("builder-c"))
	assert.Empty(t, queue.PendingBuildsForSerialKey("builder-c"))

	close(release)
}
