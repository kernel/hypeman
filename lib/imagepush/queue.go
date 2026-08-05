package imagepush

import "sync"

type queuedPush struct {
	key     string
	startFn func()
}

// pushQueue runs push jobs with a configurable concurrency limit. Jobs are
// keyed by digest+target so duplicate requests dedupe against in-flight work.
type pushQueue struct {
	maxConcurrent int
	active        map[string]bool
	pending       []queuedPush
	mu            sync.Mutex
}

func newPushQueue(maxConcurrent int) *pushQueue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &pushQueue{
		maxConcurrent: maxConcurrent,
		active:        make(map[string]bool),
		pending:       make([]queuedPush, 0),
	}
}

// Enqueue adds a job to the queue. Returns the queue position: 0 if started
// immediately, >0 if queued behind other jobs. If the key is already active
// or pending, returns the existing position without re-enqueueing. When the
// job finishes, done runs after the key leaves the active set; until done
// returns, Enqueue still reports the key as in flight, so callers can rely on
// their own bookkeeping being torn down only after the queue is done with the
// key.
func (q *pushQueue) Enqueue(key string, startFn func(), done func()) int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.active[key] {
		return 0
	}
	for i, job := range q.pending {
		if job.key == key {
			return i + 1
		}
	}

	wrappedFn := func() {
		startFn()
		q.complete(key, done)
	}

	if len(q.active) < q.maxConcurrent {
		q.active[key] = true
		go wrappedFn()
		return 0
	}

	q.pending = append(q.pending, queuedPush{key: key, startFn: wrappedFn})
	return len(q.pending)
}

// complete removes the key from the active set, starts the next pending job
// if there is capacity, and then runs done. done runs after the key is no
// longer active so a concurrent Enqueue cannot fall into the gap between
// "job finished" and "queue slot released".
func (q *pushQueue) complete(key string, done func()) {
	q.mu.Lock()
	delete(q.active, key)

	var next *queuedPush
	if len(q.pending) > 0 && len(q.active) < q.maxConcurrent {
		nextJob := q.pending[0]
		q.pending = q.pending[1:]
		q.active[nextJob.key] = true
		next = &nextJob
	}
	q.mu.Unlock()

	if next != nil {
		go next.startFn()
	}
	if done != nil {
		done()
	}
}

// MarkComplete releases the key without running a completion callback.
func (q *pushQueue) MarkComplete(key string) {
	q.complete(key, nil)
}

// GetPosition returns nil if the key is active or unknown, otherwise its
// 1-based position in the pending queue.
func (q *pushQueue) GetPosition(key string) *int {
	q.mu.Lock()
	defer q.mu.Unlock()

	if q.active[key] {
		return nil
	}
	for i, job := range q.pending {
		if job.key == key {
			pos := i + 1
			return &pos
		}
	}
	return nil
}

// ActiveKeys returns the keys of currently running jobs.
func (q *pushQueue) ActiveKeys() []string {
	q.mu.Lock()
	defer q.mu.Unlock()

	keys := make([]string, 0, len(q.active)+len(q.pending))
	for key := range q.active {
		keys = append(keys, key)
	}
	for _, job := range q.pending {
		keys = append(keys, job.key)
	}
	return keys
}
