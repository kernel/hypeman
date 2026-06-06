package uffdgraduate

import (
	"context"
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
	gradCh    chan string
	err       error
}

func newFakeStore(target string, insts ...Instance) *fakeStore {
	return &fakeStore{insts: insts, target: target, gradCh: make(chan string, 16)}
}

func (f *fakeStore) ListPagerInstances(context.Context) ([]Instance, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Instance(nil), f.insts...), f.err
}

func (f *fakeStore) GraduateInstance(_ context.Context, id string) error {
	f.mu.Lock()
	if f.err != nil {
		err := f.err
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

func TestSelectCandidatesCapKeepsNewest(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new",
		Instance{ID: "old1", PagerVersion: "new"},
		Instance{ID: "old2", PagerVersion: "new"},
	)
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute, MaxActiveSessions: 1}, clock)
	c.firstSeen["old1"] = base.Add(-30 * time.Minute)
	c.firstSeen["old2"] = base.Add(-20 * time.Minute)

	got := ids(c.selectCandidatesLocked(store.insts, "new", clock.Now()))
	// Over the ceiling by one; only the oldest is graduated, newest stays warm.
	if len(got) != 1 || got[0] != "old1" {
		t.Fatalf("candidates = %v, want [old1]", got)
	}
}

func TestSelectCandidatesOutdatedAlwaysGraduates(t *testing.T) {
	base := time.Unix(1_700_000_000, 0)
	clock := &fakeClock{t: base}
	store := newFakeStore("new",
		Instance{ID: "outdated", PagerVersion: "old"},
		Instance{ID: "current", PagerVersion: "new"},
	)
	// Ceiling above the live count, so capacity alone would graduate nothing.
	c := newTestController(store, Config{Enabled: true, MinSessionAge: 10 * time.Minute, MaxActiveSessions: 5}, clock)
	c.firstSeen["outdated"] = base.Add(-20 * time.Minute)
	c.firstSeen["current"] = base.Add(-20 * time.Minute)

	got := ids(c.selectCandidatesLocked(store.insts, "new", clock.Now()))
	if len(got) != 1 || got[0] != "outdated" {
		t.Fatalf("candidates = %v, want [outdated]", got)
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
