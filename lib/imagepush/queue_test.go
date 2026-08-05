package imagepush

import (
	"sync"
	"testing"
	"time"
)

func TestPushQueueConcurrencyLimit(t *testing.T) {
	q := newPushQueue(1)

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

	posA := q.Enqueue("a", startFn(true))
	posB := q.Enqueue("b", startFn(false))
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

func TestPushQueueDedupesByKey(t *testing.T) {
	q := newPushQueue(1)
	release := make(chan struct{})

	blocked := func() { <-release }
	started := make(chan struct{}, 1)
	first := func() {
		started <- struct{}{}
		<-release
	}

	q.Enqueue("same", first)
	<-started

	pos := q.Enqueue("same", blocked)
	if pos != 0 {
		t.Errorf("duplicate enqueue of active key = %d, want 0", pos)
	}
	pos = q.Enqueue("same", blocked)
	if pos != 0 {
		t.Errorf("second duplicate enqueue = %d, want 0 (still active)", pos)
	}

	close(release)
	time.Sleep(50 * time.Millisecond)

	// After completion the key is no longer tracked.
	if pos := q.GetPosition("same"); pos != nil {
		t.Errorf("GetPosition after completion = %v, want nil", pos)
	}
}

func TestPushQueueActiveKeys(t *testing.T) {
	q := newPushQueue(1)
	release := make(chan struct{})

	q.Enqueue("a", func() { <-release })
	q.Enqueue("b", func() {})

	keys := q.ActiveKeys()
	if len(keys) != 2 {
		t.Fatalf("ActiveKeys = %v, want both a and b", keys)
	}

	close(release)
	time.Sleep(50 * time.Millisecond)

	keys = q.ActiveKeys()
	if len(keys) != 0 {
		t.Errorf("ActiveKeys after completion = %v, want empty", keys)
	}
}
