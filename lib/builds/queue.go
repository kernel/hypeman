package builds

import "sync"

// QueuedBuild represents a build waiting to be executed
type QueuedBuild struct {
	BuildID string
	Request CreateBuildRequest
	// SerialKey groups builds that must not run concurrently (e.g. builds
	// sharing a cache volume). A queued build with a serial key only starts
	// once no active build holds the same key, so it never occupies a
	// concurrency slot while waiting on another build of its group.
	SerialKey string
	StartFn   func()
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
	maxConcurrent    int
	active           map[string]bool
	activeSerialKeys map[string]string // build ID -> serial key
	pending          []QueuedBuild
	mu               sync.Mutex
}

// NewBuildQueue creates a new build queue with the given concurrency limit
func NewBuildQueue(maxConcurrent int) *BuildQueue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &BuildQueue{
		maxConcurrent:    maxConcurrent,
		active:           make(map[string]bool),
		activeSerialKeys: make(map[string]string),
		pending:          make([]QueuedBuild, 0),
	}
}

// Enqueue adds a build to the queue. Returns queue position (0 if started
// immediately, >0 if queued among pending builds that can currently start).
// If the build is already building or queued, returns its current position without re-enqueueing.
func (q *BuildQueue) Enqueue(buildID string, req CreateBuildRequest, startFn func()) int {
	return q.EnqueueSerial(buildID, req, "", startFn)
}

// EnqueueSerial is Enqueue with a serial key: builds sharing a non-empty key
// are serialized so a waiting build stays pending instead of occupying a
// concurrency slot while blocked on another build of its group.
func (q *BuildQueue) EnqueueSerial(buildID string, req CreateBuildRequest, serialKey string, startFn func()) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Check if already building (position 0, actively running)
	if q.active[buildID] {
		return 0
	}

	// Check if already in pending queue
	for _, build := range q.pending {
		if build.BuildID == buildID {
			if pos := q.pendingPositionLocked(buildID); pos != nil {
				return *pos
			}
			return 1
		}
	}

	// Wrap the function to auto-complete
	wrappedFn := func() {
		defer q.MarkComplete(buildID)
		startFn()
	}

	build := QueuedBuild{
		BuildID:   buildID,
		Request:   req,
		SerialKey: serialKey,
		StartFn:   wrappedFn,
	}

	// Start immediately if under concurrency limit and the serial key is free
	if q.canStartLocked(serialKey) {
		q.startLocked(build)
		return 0
	}

	// Otherwise queue it
	q.pending = append(q.pending, build)
	if pos := q.pendingPositionLocked(buildID); pos != nil {
		return *pos
	}
	return 1
}

// canStartLocked reports whether a build with the given serial key may start.
func (q *BuildQueue) canStartLocked(serialKey string) bool {
	if len(q.active) >= q.maxConcurrent {
		return false
	}
	if serialKey == "" {
		return true
	}
	for _, key := range q.activeSerialKeys {
		if key == serialKey {
			return false
		}
	}
	return true
}

// startLocked marks a build active and launches it.
func (q *BuildQueue) startLocked(build QueuedBuild) {
	q.active[build.BuildID] = true
	if build.SerialKey != "" {
		q.activeSerialKeys[build.BuildID] = build.SerialKey
	}
	go build.StartFn()
}

// MarkComplete marks a build as complete and starts pending builds while
// capacity and serial keys allow. A pending build whose serial key is held
// is skipped so later builds can proceed.
func (q *BuildQueue) MarkComplete(buildID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.active, buildID)
	delete(q.activeSerialKeys, buildID)

	for len(q.active) < q.maxConcurrent {
		idx := -1
		for i, build := range q.pending {
			if q.serialKeyFreeLocked(build.SerialKey) {
				idx = i
				break
			}
		}
		if idx == -1 {
			return
		}
		next := q.pending[idx]
		q.pending = append(q.pending[:idx], q.pending[idx+1:]...)
		q.startLocked(next)
	}
}

// serialKeyFreeLocked reports whether no active build holds the serial key.
func (q *BuildQueue) serialKeyFreeLocked(serialKey string) bool {
	if serialKey == "" {
		return true
	}
	for _, key := range q.activeSerialKeys {
		if key == serialKey {
			return false
		}
	}
	return true
}

// GetPosition returns the queue position for a build.
// Returns nil if the build is actively running or not in queue.
func (q *BuildQueue) GetPosition(buildID string) *int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.active[buildID] {
		return nil // Actively running, not queued
	}

	return q.pendingPositionLocked(buildID)
}

// pendingPositionLocked returns the pending queue position for buildID.
// Position counts only pending builds whose serial keys are currently free.
// Builds blocked by active serial keys are skipped so later startable builds
// are not reported behind work that cannot start yet.
func (q *BuildQueue) pendingPositionLocked(buildID string) *int {
	pos := 0
	for _, build := range q.pending {
		blocked := !q.serialKeyFreeLocked(build.SerialKey)
		if build.BuildID == buildID {
			pos++
			return &pos
		}
		if blocked {
			continue
		}
		pos++
	}
	return nil
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
