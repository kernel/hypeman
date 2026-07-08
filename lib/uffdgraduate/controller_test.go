package uffdgraduate

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type fakeStore struct {
	mu        sync.Mutex
	insts     []Instance
	target    string
	graduated []string
	attempts  int
	gradCh    chan string
	listErr   error
	gradErr   error
}

func newFakeStore(target string, insts ...Instance) *fakeStore {
	return &fakeStore{insts: insts, target: target, gradCh: make(chan string, 16)}
}

func (f *fakeStore) ListPagerInstances(context.Context) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Instance(nil), f.insts...), nil
}

func (f *fakeStore) GraduateInstance(_ context.Context, id string) error {
	f.mu.Lock()
	f.attempts++
	if f.gradErr != nil {
		err := f.gradErr
		f.mu.Unlock()
		return err
	}
	f.graduated = append(f.graduated, id)
	rm := f.insts[:0]
	for _, inst := range f.insts {
		if inst.ID != id {
			rm = append(rm, inst)
		}
	}
	f.insts = rm
	f.mu.Unlock()
	f.gradCh <- id
	return nil
}

func (f *fakeStore) TargetVersion() string { return f.target }

func (f *fakeStore) attemptCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.attempts
}

func (f *fakeStore) graduatedIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.graduated...)
}

func (f *fakeStore) setGradErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gradErr = err
}

func newTestController(store InstanceStore, cfg Config, clock *fakeClock) *Controller {
	return NewController(store, cfg, ControllerOptions{Now: clock.Now})
}

func ids(insts []Instance) []string {
	out := make([]string, len(insts))
	for i, inst := range insts {
		out[i] = inst.ID
	}
	return out
}

func TestSelectCandidatesTimeBasedWeaning(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new",
		Instance{ID: "a", PagerVersion: "old"},
		Instance{ID: "b", PagerVersion: "new"},
		Instance{ID: "fresh", PagerVersion: "new"},
	)
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute}, clock)

	c.firstSeen["a"] = base.Add(-20 * time.Minute)
	c.firstSeen["b"] = base.Add(-15 * time.Minute)
	c.firstSeen["fresh"] = base.Add(-time.Minute)

	got := ids(c.selectCandidatesLocked(store.insts, "new", clock.Now()))
	// Both soaked instances graduate; the outdated one is ordered first; the
	// fresh instance is excluded.
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("candidates = %v, want [a b]", got)
	}
}

func TestSelectCandidatesSkipsRecentFailure(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new", Instance{ID: "vm1", PagerVersion: "new"})
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute}, clock)

	c.firstSeen["vm1"] = base.Add(-20 * time.Minute)
	c.lastFailure["vm1"] = base.Add(-failureBackoff / 2)

	if got := ids(c.selectCandidatesLocked(store.insts, "new", clock.Now())); len(got) != 0 {
		t.Fatalf("candidates = %v, want none within backoff", got)
	}

	clock.advance(failureBackoff)
	if got := ids(c.selectCandidatesLocked(store.insts, "new", clock.Now())); len(got) != 1 || got[0] != "vm1" {
		t.Fatalf("candidates = %v, want [vm1] after backoff", got)
	}
}

func TestScanRespectsSoak(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new", Instance{ID: "vm1", PagerVersion: "old"})
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute, MaxConcurrent: 1}, clock)

	c.scan(context.Background()) // records first-seen, age 0 -> no graduation
	select {
	case id := <-store.gradCh:
		t.Fatalf("unexpected graduation before soak: %s", id)
	case <-time.After(50 * time.Millisecond):
	}

	clock.advance(11 * time.Minute)
	c.scan(context.Background())
	select {
	case id := <-store.gradCh:
		if id != "vm1" {
			t.Fatalf("graduated %s, want vm1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected graduation after soak")
	}

	// First-seen is dropped on success so a rebind restarts the soak.
	c.wg.Wait()
	c.mu.Lock()
	_, tracked := c.firstSeen["vm1"]
	c.mu.Unlock()
	if tracked {
		t.Fatal("expected first-seen cleared after successful graduation")
	}
}

func TestScanListFailure(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new", Instance{ID: "vm1", PagerVersion: "new"})
	store.listErr = errors.New("list boom")
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute}, clock)

	c.scan(context.Background())
	c.wg.Wait()
	if n := store.attemptCount(); n != 0 {
		t.Fatalf("attempts = %d, want 0 when listing fails", n)
	}
	c.mu.Lock()
	tracked := len(c.firstSeen)
	c.mu.Unlock()
	if tracked != 0 {
		t.Fatalf("firstSeen tracked %d instances from a failed list", tracked)
	}
}

func TestScanGraduateFailureBacksOff(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new", Instance{ID: "vm1", PagerVersion: "new"})
	store.setGradErr(errors.New("populate boom"))
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute, MaxConcurrent: 1}, clock)

	c.scan(context.Background())
	clock.advance(11 * time.Minute)
	c.scan(context.Background())
	c.wg.Wait()
	if n := store.attemptCount(); n != 1 {
		t.Fatalf("attempts = %d, want 1", n)
	}

	// Within the backoff window the instance is left alone.
	clock.advance(time.Minute)
	c.scan(context.Background())
	c.wg.Wait()
	if n := store.attemptCount(); n != 1 {
		t.Fatalf("attempts = %d, want still 1 within backoff", n)
	}

	// After the backoff it is retried, and success clears the failure record.
	store.setGradErr(nil)
	clock.advance(failureBackoff)
	c.scan(context.Background())
	select {
	case id := <-store.gradCh:
		if id != "vm1" {
			t.Fatalf("graduated %s, want vm1", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected retry after backoff")
	}
	c.wg.Wait()
	c.mu.Lock()
	_, failed := c.lastFailure["vm1"]
	c.mu.Unlock()
	if failed {
		t.Fatal("expected lastFailure cleared after successful graduation")
	}
}

func TestScanMaxConcurrent(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new",
		Instance{ID: "a", PagerVersion: "new"},
		Instance{ID: "b", PagerVersion: "new"},
		Instance{ID: "c", PagerVersion: "new"},
	)
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute, MaxConcurrent: 2}, clock)

	c.scan(context.Background())
	clock.advance(11 * time.Minute)
	c.scan(context.Background())
	c.wg.Wait()
	if got := store.graduatedIDs(); len(got) != 2 {
		t.Fatalf("graduated %v, want exactly 2 in one scan", got)
	}

	c.scan(context.Background())
	c.wg.Wait()
	if got := store.graduatedIDs(); len(got) != 3 {
		t.Fatalf("graduated %v, want all 3 after the next scan", got)
	}
}
