package builds

import (
	"context"
	"sync"
)

// StartFunc executes a queued build. The context carries the enqueue-time
// values (e.g. trace context) and is cancelled when the queue shuts down.
type StartFunc func(ctx context.Context)

// QueuedBuild represents a build waiting to be executed
type QueuedBuild struct {
	BuildID string
	Request CreateBuildRequest
	// SerialKey groups builds that must not run concurrently (builds
	// sharing a builder disk). A queued build with a serial key only starts
	// once no active build holds the same key, so it never occupies a
	// concurrency slot while waiting on another build of its group.
	SerialKey string
	StartFn   StartFunc
	// ctx carries the enqueue-time context values into the run; cancel
	// releases it and aborts the run on shutdown.
	ctx    context.Context
	cancel context.CancelFunc
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
	activeSerialKeys map[string]string             // build ID -> serial key
	activeCancels    map[string]context.CancelFunc // build ID -> run cancel
	pending          []QueuedBuild
	wg               sync.WaitGroup // tracks active build goroutines
	shutdown         bool
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
		activeCancels:    make(map[string]context.CancelFunc),
		pending:          make([]QueuedBuild, 0),
	}
}

// Enqueue adds a build to the queue. Returns queue position (0 if started
// immediately, >0 if queued among pending builds that can currently start).
// If the build is already building or queued, returns its current position without re-enqueueing.
//
// The ctx supplies values (trace context, logger) to the run and bounds its
// lifetime together with Shutdown: the run is cancelled when ctx is
// cancelled or the queue shuts down. Callers that want the build to survive
// request cancellation should pass context.WithoutCancel(ctx).
func (q *BuildQueue) Enqueue(ctx context.Context, buildID string, req CreateBuildRequest, startFn StartFunc) int {
	return q.EnqueueSerial(ctx, buildID, req, "", startFn)
}

// EnqueueSerial is Enqueue with a serial key: builds sharing a non-empty key
// are serialized so a waiting build stays pending instead of occupying a
// concurrency slot while blocked on another build of its group.
func (q *BuildQueue) EnqueueSerial(ctx context.Context, buildID string, req CreateBuildRequest, serialKey string, startFn StartFunc) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Check if already building (position 0, actively running)
	if q.active[buildID] {
		return 0
	}

	// Check if already in pending queue
	for _, build := range q.pending {
		if build.BuildID == buildID {
			return *q.pendingPositionLocked(buildID)
		}
	}

	runCtx, cancel := context.WithCancel(ctx)
	build := QueuedBuild{
		BuildID:   buildID,
		Request:   req,
		SerialKey: serialKey,
		StartFn:   startFn,
		ctx:       runCtx,
		cancel:    cancel,
	}

	// Start immediately if under concurrency limit and the serial key is free
	if q.canStartLocked(serialKey) {
		q.startLocked(build)
		return 0
	}

	// Otherwise queue it. After Shutdown the build stays pending and never
	// starts; its on-disk metadata lets startup recovery re-enqueue it.
	q.pending = append(q.pending, build)
	return *q.pendingPositionLocked(buildID)
}

// canStartLocked reports whether a build with the given serial key may start.
func (q *BuildQueue) canStartLocked(serialKey string) bool {
	if q.shutdown {
		return false
	}
	if len(q.active) >= q.maxConcurrent {
		return false
	}
	return q.serialKeyFreeLocked(serialKey)
}

// startLocked marks a build active and launches it.
func (q *BuildQueue) startLocked(build QueuedBuild) {
	q.active[build.BuildID] = true
	if build.SerialKey != "" {
		q.activeSerialKeys[build.BuildID] = build.SerialKey
	}
	q.activeCancels[build.BuildID] = build.cancel
	q.wg.Add(1)
	go func() {
		defer q.wg.Done()
		defer build.cancel()
		defer q.MarkComplete(build.BuildID)
		build.StartFn(build.ctx)
	}()
}

// MarkComplete marks a build as complete and starts pending builds while
// capacity and serial keys allow. A pending build whose serial key is held
// is skipped so later builds can proceed.
func (q *BuildQueue) MarkComplete(buildID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.active, buildID)
	delete(q.activeSerialKeys, buildID)
	delete(q.activeCancels, buildID)
	q.startPendingLocked()
}

// ReleaseSerialKey frees the serial key held by an active build without
// marking it complete, so a same-key pending build can start once the
// active build no longer needs exclusivity (after it releases the builder
// disk but before it finishes post-build work). The successor starts even
// when every global slot is taken: the releasing build is past its
// exclusive phase, so serialized builds would otherwise never overlap at
// MaxConcurrentBuilds=1.
func (q *BuildQueue) ReleaseSerialKey(buildID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	key, held := q.activeSerialKeys[buildID]
	delete(q.activeSerialKeys, buildID)
	if held && !q.shutdown {
		for i, build := range q.pending {
			if build.SerialKey == key {
				q.pending = append(q.pending[:i], q.pending[i+1:]...)
				q.startLocked(build)
				break
			}
		}
	}
	q.startPendingLocked()
}

// startPendingLocked starts pending builds while capacity and serial keys
// allow.
func (q *BuildQueue) startPendingLocked() {
	if q.shutdown {
		return
	}
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

// pendingPositionLocked returns the physical pending queue position for
// buildID. Scheduling may skip a blocked serial key, but positions retain the
// existing submission-order semantics for every build.
func (q *BuildQueue) pendingPositionLocked(buildID string) *int {
	for i, build := range q.pending {
		if build.BuildID == buildID {
			pos := i + 1
			return &pos
		}
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
			build.cancel()
			return true
		}
	}

	return false
}

// Shutdown stops scheduling new work, cancels every active build's run
// context, and waits for their goroutines to return. Pending builds are left
// unstarted; their persisted metadata lets startup recovery re-enqueue them
// on the next boot. Returns ctx.Err() if the wait outlives ctx.
func (q *BuildQueue) Shutdown(ctx context.Context) error {
	q.mu.Lock()
	if q.shutdown {
		q.mu.Unlock()
		return nil
	}
	q.shutdown = true
	for _, build := range q.pending {
		build.cancel()
	}
	cancels := make([]context.CancelFunc, 0, len(q.activeCancels))
	for _, cancel := range q.activeCancels {
		cancels = append(cancels, cancel)
	}
	q.mu.Unlock()

	for _, cancel := range cancels {
		cancel()
	}

	done := make(chan struct{})
	go func() {
		q.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// HasSerialKey reports whether an active or pending build uses serialKey.
// Both states are inspected under one lock so pending-to-active transitions
// cannot briefly appear idle to lifecycle guards.
func (q *BuildQueue) HasSerialKey(serialKey string) bool {
	if serialKey == "" {
		return false
	}
	q.mu.Lock()
	defer q.mu.Unlock()

	for _, key := range q.activeSerialKeys {
		if key == serialKey {
			return true
		}
	}
	for _, build := range q.pending {
		if build.SerialKey == serialKey {
			return true
		}
	}
	return false
}

// ActiveBuildForSerialKey returns the ID of the active build holding
// serialKey, or nil when no active build holds it.
func (q *BuildQueue) ActiveBuildForSerialKey(serialKey string) *string {
	q.mu.Lock()
	defer q.mu.Unlock()

	for buildID, key := range q.activeSerialKeys {
		if key == serialKey {
			id := buildID
			return &id
		}
	}
	return nil
}

// PendingBuildsForSerialKey returns the IDs of pending builds with
// serialKey, oldest first.
func (q *BuildQueue) PendingBuildsForSerialKey(serialKey string) []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	ids := make([]string, 0)
	for _, build := range q.pending {
		if build.SerialKey == serialKey {
			ids = append(ids, build.BuildID)
		}
	}
	return ids
}
