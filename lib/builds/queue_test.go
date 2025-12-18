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
	pos := queue.Enqueue("build-1", CreateBuildRequest{Runtime: "nodejs20"}, func() {
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

