package instances

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSubscribers_NotifyDelivers(t *testing.T) {
	s := newSubscribers()
	ch, unsub := s.Subscribe("inst-1")
	defer unsub()

	s.Notify("inst-1", StateChange{State: StateRunning})

	select {
	case sc := <-ch:
		assert.Equal(t, StateRunning, sc.State)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for state change")
	}
}

func TestSubscribers_MultipleSubscribers(t *testing.T) {
	s := newSubscribers()
	ch1, unsub1 := s.Subscribe("inst-1")
	defer unsub1()
	ch2, unsub2 := s.Subscribe("inst-1")
	defer unsub2()

	s.Notify("inst-1", StateChange{State: StateStopped})

	for _, ch := range []<-chan StateChange{ch1, ch2} {
		select {
		case sc := <-ch:
			assert.Equal(t, StateStopped, sc.State)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for state change")
		}
	}
}

func TestSubscribers_UnsubscribeStopsDelivery(t *testing.T) {
	s := newSubscribers()
	ch, unsub := s.Subscribe("inst-1")
	unsub()

	// Channel should be closed after unsubscribe.
	_, ok := <-ch
	assert.False(t, ok, "channel should be closed after unsubscribe")
}

func TestSubscribers_DifferentInstancesIsolated(t *testing.T) {
	s := newSubscribers()
	ch1, unsub1 := s.Subscribe("inst-1")
	defer unsub1()
	ch2, unsub2 := s.Subscribe("inst-2")
	defer unsub2()

	s.Notify("inst-1", StateChange{State: StateRunning})

	select {
	case sc := <-ch1:
		assert.Equal(t, StateRunning, sc.State)
	case <-time.After(time.Second):
		t.Fatal("inst-1 subscriber should have received event")
	}

	select {
	case <-ch2:
		t.Fatal("inst-2 subscriber should not have received event")
	case <-time.After(50 * time.Millisecond):
		// expected
	}
}

func TestSubscribers_CloseAll(t *testing.T) {
	s := newSubscribers()
	ch1, _ := s.Subscribe("inst-1")
	ch2, _ := s.Subscribe("inst-1")

	s.CloseAll("inst-1")

	_, ok1 := <-ch1
	_, ok2 := <-ch2
	assert.False(t, ok1, "ch1 should be closed")
	assert.False(t, ok2, "ch2 should be closed")
}

func TestSubscribers_NotifyNoSubscribersNoPanic(t *testing.T) {
	s := newSubscribers()
	require.NotPanics(t, func() {
		s.Notify("no-such-instance", StateChange{State: StateRunning})
	})
}
