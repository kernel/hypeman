package queue

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestConcurrencyLimit(t *testing.T) {
	q := New(1)

	var mu sync.Mutex
	running := 0
	maxRunning := 0
	release := make(chan struct{})
	done := make(chan struct{}, 2)

	startFn := func(block bool) func() {
		return func() {
			mu.Lock()
			running++
			if running > maxRunning {
				maxRunning = running
			}
			mu.Unlock()

			if block {
				<-release
			}

			mu.Lock()
			running--
			mu.Unlock()
			done <- struct{}{}
		}
	}

	posA := q.Enqueue("a", startFn(true), nil)
	posB := q.Enqueue("b", startFn(false), nil)
	if posA != 0 {
		t.Errorf("posA = %d, want 0 (started immediately)", posA)
	}
	if posB != 1 {
		t.Errorf("posB = %d, want 1 (queued behind a)", posB)
	}

	if pos := q.GetPosition("b"); pos == nil || *pos != 1 {
		t.Errorf("GetPosition(b) = %v, want 1", pos)
	}
	if pos := q.GetPosition("a"); pos != nil {
		t.Errorf("GetPosition(a) = %v, want nil (active)", pos)
	}

	close(release)
	for i := 0; i < 2; i++ {
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("timed out waiting for jobs")
		}
	}

	if maxRunning != 1 {
		t.Errorf("maxRunning = %d, want 1", maxRunning)
	}
	if pos := q.GetPosition("b"); pos != nil {
		t.Errorf("GetPosition(b) after completion = %v, want nil", pos)
	}
}

func TestDedupesByKey(t *testing.T) {
	q := New(1)
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	done := make(chan struct{})
	var ran int64

	q.Enqueue("same", func() {
		started <- struct{}{}
		atomic.AddInt64(&ran, 1)
		<-release
	}, func() { close(done) })
	<-started

	// Duplicate enqueues of an active key must not start another job.
	q.Enqueue("same", func() { atomic.AddInt64(&ran, 1) }, nil)
	q.Enqueue("same", func() { atomic.AddInt64(&ran, 1) }, nil)
	if pos := q.GetPosition("same"); pos != nil {
		t.Errorf("GetPosition(same) = %v, want nil (active)", pos)
	}

	close(release)
	// The completion hook runs after the key leaves the active set, so once
	// it fires the job is fully drained — no sleep-based guesswork.
	<-done

	if atomic.LoadInt64(&ran) != 1 {
		t.Errorf("job ran %d times, want 1", ran)
	}
	if pos := q.GetPosition("same"); pos != nil {
		t.Errorf("GetPosition after completion = %v, want nil", pos)
	}
}

// TestDoneRunsAfterKeyReleased guards the completion-hook ordering: done must
// fire only after the key has left the active set, otherwise a caller's
// "job finished" bookkeeping would race a concurrent re-enqueue of the key.
func TestDoneRunsAfterKeyReleased(t *testing.T) {
	q := New(1)

	release := make(chan struct{})
	stateAtDone := make(chan int, 1)
	doneSampled := make(chan struct{})

	q.Enqueue("a", func() { <-release }, func() {
		stateAtDone <- q.QueueLength()
		close(doneSampled)
	})
	// "b" occupies the freed slot when "a" completes, so if the key "a" were
	// still tracked at done time the queue would report length 2 (a + b). It
	// blocks until the done hook has sampled so it cannot finish early and
	// flake the assertion.
	q.Enqueue("b", func() { <-doneSampled }, nil)

	close(release)
	select {
	case n := <-stateAtDone:
		if n != 1 {
			t.Errorf("queue length when done ran = %d, want 1 (key a released, only b tracked)", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("done never ran")
	}

	deadline := time.Now().Add(5 * time.Second)
	for q.QueueLength() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("b never completed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
