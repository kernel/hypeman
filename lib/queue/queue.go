// Package queue provides a minimal in-memory bounded queue for keyed work
// with dedup: jobs are identified by a key, and enqueueing a key that is
// already active or pending returns its position instead of re-enqueueing.
package queue

import "sync"

type job struct {
	key     string
	startFn func()
}

// Queue runs jobs with a configurable concurrency limit. Queue state is
// in-memory; callers persist their own job metadata and recover on startup.
type Queue struct {
	maxConcurrent int
	active        map[string]bool
	pending       []job
	mu            sync.Mutex
}

// New creates a Queue with the given concurrency limit (minimum 1).
func New(maxConcurrent int) *Queue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Queue{
		maxConcurrent: maxConcurrent,
		active:        make(map[string]bool),
		pending:       make([]job, 0),
	}
}

// Enqueue adds a job keyed by key. Returns the queue position: 0 if it
// started immediately, >0 if queued behind other jobs. If the key is already
// active or pending, Enqueue dedups and returns its current position without
// re-enqueueing. A completion hook may be provided; when non-nil it runs
// after the key leaves the active set (so a caller's bookkeeping for the key
// is torn down only once the queue is done with it), but only when this call
// actually started the job — a dedup'd enqueue does not run it.
func (q *Queue) Enqueue(key string, startFn func(), done func()) int {
	return q.enqueue(key, startFn, done, false)
}

// EnqueueSuccessor queues a new job behind an active job with the same key.
// Pending jobs with the same key are still deduplicated. This is useful when
// the caller has replaced the persisted work for a key while the previous job
// is finishing.
func (q *Queue) EnqueueSuccessor(key string, startFn func(), done func()) int {
	return q.enqueue(key, startFn, done, true)
}

func (q *Queue) enqueue(key string, startFn func(), done func(), allowActive bool) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.active[key] && !allowActive {
		return 0
	}
	for i, j := range q.pending {
		if j.key == key {
			return i + 1
		}
	}

	wrappedFn := func() {
		// complete runs first (last-registered defer runs first), so done
		// only fires after the key has left the active set.
		if done != nil {
			defer done()
		}
		defer q.complete(key)
		startFn()
	}

	if len(q.active) < q.maxConcurrent && !q.active[key] {
		q.active[key] = true
		// The key enters the active set before the goroutine launches, both
		// under the lock, so a concurrent Enqueue always observes the slot and
		// dedups. The goroutine may run to completion before Enqueue returns,
		// but its deferred complete then blocks on q.mu, so every state
		// transition still serializes after this call.
		go wrappedFn()
		return 0
	}

	q.pending = append(q.pending, job{key: key, startFn: wrappedFn})
	return len(q.pending)
}

// complete removes the key from the active set and starts the next pending
// job if there is capacity. Pending successors for an active key are skipped
// until that key completes.
func (q *Queue) complete(key string) {
	q.mu.Lock()
	delete(q.active, key)

	var next *job
	if len(q.active) < q.maxConcurrent {
		for i, pending := range q.pending {
			if q.active[pending.key] {
				continue
			}
			q.pending = append(q.pending[:i], q.pending[i+1:]...)
			q.active[pending.key] = true
			next = &pending
			break
		}
	}
	q.mu.Unlock()

	if next != nil {
		go next.startFn()
	}
}

// GetPosition returns nil if the key is unknown or active without a
// successor, otherwise its 1-based position in the pending queue.
func (q *Queue) GetPosition(key string) *int {
	q.mu.Lock()
	defer q.mu.Unlock()

	for i, j := range q.pending {
		if j.key == key {
			pos := i + 1
			return &pos
		}
	}
	return nil
}

// QueueLength returns the number of tracked jobs (active + pending).
func (q *Queue) QueueLength() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.active) + len(q.pending)
}
