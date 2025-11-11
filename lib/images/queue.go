package images

import "sync"

type QueuedBuild struct {
	ImageName string
	Request   CreateImageRequest
	StartFn   func()
}

// BuildQueue manages concurrent image builds with a configurable limit
type BuildQueue struct {
	maxConcurrent int
	active        map[string]bool
	pending       []QueuedBuild
	mu            sync.Mutex
}

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
// If the image is already building or queued, returns its current position without re-enqueueing.
func (q *BuildQueue) Enqueue(imageName string, req CreateImageRequest, startFn func()) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	// Check if already building (position 0, actively running)
	if q.active[imageName] {
		return 0
	}

	// Check if already in pending queue
	for i, build := range q.pending {
		if build.ImageName == imageName {
			return i + 1 // Return existing queue position
		}
	}

	// Wrap the function to auto-complete
	wrappedFn := func() {
		defer q.MarkComplete(imageName)
		startFn()
	}

	build := QueuedBuild{
		ImageName: imageName,
		Request:   req,
		StartFn:   wrappedFn,
	}

	if len(q.active) < q.maxConcurrent {
		q.active[imageName] = true
		go wrappedFn()
		return 0
	}

	q.pending = append(q.pending, build)
	return len(q.pending)
}

func (q *BuildQueue) MarkComplete(imageName string) {
	q.mu.Lock()
	defer q.mu.Unlock()

	delete(q.active, imageName)

	if len(q.pending) > 0 && len(q.active) < q.maxConcurrent {
		next := q.pending[0]
		q.pending = q.pending[1:]
		q.active[next.ImageName] = true
		go next.StartFn()
	}
}

func (q *BuildQueue) GetPosition(imageName string) *int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.active[imageName] {
		return nil
	}

	for i, build := range q.pending {
		if build.ImageName == imageName {
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

