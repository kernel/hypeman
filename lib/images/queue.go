package images

import (
	"sync"

	"github.com/onkernel/hypeman/lib/oapi"
)

// QueuedBuild represents a build waiting in queue
type QueuedBuild struct {
	ImageID string
	Request oapi.CreateImageRequest
	StartFn func() // Callback to start the build
}

// BuildQueue manages concurrent image builds with a configurable limit
type BuildQueue struct {
	maxConcurrent int
	active        map[string]bool // imageID -> is building
	pending       []QueuedBuild
	mu            sync.Mutex
}

// NewBuildQueue creates a new build queue with max concurrent limit
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

// Enqueue adds a build to the queue and returns queue position
// Returns 0 if build starts immediately, >0 if queued
func (q *BuildQueue) Enqueue(imageID string, req oapi.CreateImageRequest, startFn func()) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	build := QueuedBuild{
		ImageID: imageID,
		Request: req,
		StartFn: startFn,
	}

	// If under limit, start immediately
	if len(q.active) < q.maxConcurrent {
		q.active[imageID] = true
		go startFn()
		return 0 // Building now, not queued
	}

	// Otherwise, add to queue
	q.pending = append(q.pending, build)
	return len(q.pending) // Position in queue
}

// MarkComplete marks a build as complete and starts the next queued build
func (q *BuildQueue) MarkComplete(imageID string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.active, imageID)

	// Try to start next build
	if len(q.pending) > 0 && len(q.active) < q.maxConcurrent {
		next := q.pending[0]
		q.pending = q.pending[1:]
		q.active[next.ImageID] = true
		go next.StartFn()
	}
}

// GetPosition returns the queue position for an image
// Returns nil if not in queue (either building or complete)
func (q *BuildQueue) GetPosition(imageID string) *int {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Check if actively building
	if q.active[imageID] {
		return nil
	}

	// Check if in queue
	for i, build := range q.pending {
		if build.ImageID == imageID {
			pos := i + 1
			return &pos
		}
	}

	return nil
}

// ActiveCount returns number of actively building images
func (q *BuildQueue) ActiveCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.active)
}

// PendingCount returns number of queued builds
func (q *BuildQueue) PendingCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.pending)
}

