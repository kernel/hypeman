package builds

import "sync"

// QueuedBuild represents a build waiting to be executed
type QueuedBuild struct {
	BuildID string
	Request CreateBuildRequest
	StartFn func()
}

// BuildQueue manages concurrent builds with a configurable limit.
// Following the pattern from lib/images/queue.go.
//
// Design notes (see plan for full context):
// - Queue state is in-memory (lost on restart)
// - Build metadata is persisted to disk
// - On startup, pending builds are recovered via listPendingBuilds()
//
// Future migration path if needed:
// - Add BuildQueue interface with Enqueue/Dequeue/Ack/Nack
// - Implement adapters: memoryQueue, redisQueue, natsQueue
// - Use BUILD_QUEUE_BACKEND env var to select implementation
type BuildQueue struct {
	maxConcurrent int
	active        map[string]bool
	pending       []QueuedBuild
	mu            sync.Mutex
}

// NewBuildQueue creates a new build queue with the given concurrency limit
func NewBuildQueue(maxConcurrent int) *BuildQueue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &BuildQueue{
		maxConcurrent: maxConcurrent,
		active:        make(map[string]bool),
		pending:       make([]QueuedBuild, 0),
	}
}

// Enqueue adds a build to the queue. Returns queue position (0 if started immediately, >0 if queued).
// If the build is already building or queued, returns its current position without re-enqueueing.
func (q *BuildQueue) Enqueue(buildID string, req CreateBuildRequest, startFn func()) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Check if already building (position 0, actively running)
	if q.active[buildID] {
		return 0
	}

	// Check if already in pending queue
	for i, build := range q.pending {
		if build.BuildID == buildID {
			return i + 1 // Return existing queue position
		}
	}

	// Wrap the function to auto-complete
	wrappedFn := func() {
		defer q.MarkComplete(buildID)
		startFn()
	}

	build := QueuedBuild{
		BuildID: buildID,
		Request: req,
		StartFn: wrappedFn,
	}

	// Start immediately if under concurrency limit
	if len(q.active) < q.maxConcurrent {
		q.active[buildID] = true
		go wrappedFn()
		return 0
	}

	// Otherwise queue it
	q.pending = append(q.pending, build)
	return len(q.pending)
}

// MarkComplete marks a build as complete and starts the next pending build if any
func (q *BuildQueue) MarkComplete(buildID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.active, buildID)

	// Start next pending build if we have capacity
	if len(q.pending) > 0 && len(q.active) < q.maxConcurrent {
		next := q.pending[0]
		q.pending = q.pending[1:]
		q.active[next.BuildID] = true
		go next.StartFn()
	}
}

// GetPosition returns the queue position for a build.
// Returns nil if the build is actively running or not in queue.
func (q *BuildQueue) GetPosition(buildID string) *int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.active[buildID] {
		return nil // Actively running, not queued
	}

	for i, build := range q.pending {
		if build.BuildID == buildID {
			pos := i + 1
			return &pos
		}
	}

	return nil // Not in queue
}

// Cancel removes a build from the pending queue.
// Returns true if the build was cancelled, false if it was not in the queue
// (already running or not found).
func (q *BuildQueue) Cancel(buildID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Can't cancel if actively running
	if q.active[buildID] {
		return false
	}

	// Find and remove from pending
	for i, build := range q.pending {
		if build.BuildID == buildID {
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			return true
		}
	}

	return false
}

// IsActive returns true if the build is actively running
func (q *BuildQueue) IsActive(buildID string) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.active[buildID]
}

// ActiveCount returns the number of actively building builds
func (q *BuildQueue) ActiveCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.active)
}

// PendingCount returns the number of queued builds
func (q *BuildQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

// QueueLength returns the total number of builds (active + pending)
func (q *BuildQueue) QueueLength() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.active) + len(q.pending)
}

