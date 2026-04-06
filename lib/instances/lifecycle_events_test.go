package instances

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLifecycleSubscribers_NotifyDelivers(t *testing.T) {
	s := newLifecycleSubscribers()
	ch, unsub := s.Subscribe(LifecycleEventConsumerWaitForState)
	defer unsub()

	s.Notify(context.Background(), LifecycleEvent{
		Action:     LifecycleEventStart,
		InstanceID: "inst-1",
		Instance:   &Instance{State: StateRunning},
	})

	select {
	case event := <-ch:
		assert.Equal(t, LifecycleEventStart, event.Action)
		require.NotNil(t, event.Instance)
		assert.Equal(t, StateRunning, event.Instance.State)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for lifecycle event")
	}
}

func TestLifecycleSubscribers_MultipleSubscribers(t *testing.T) {
	s := newLifecycleSubscribers()
	ch1, unsub1 := s.Subscribe(LifecycleEventConsumerWaitForState)
	defer unsub1()
	ch2, unsub2 := s.Subscribe(LifecycleEventConsumerAutoStandby)
	defer unsub2()

	s.Notify(context.Background(), LifecycleEvent{
		Action:     LifecycleEventStop,
		InstanceID: "inst-1",
		Instance:   &Instance{State: StateStopped},
	})

	for _, ch := range []<-chan LifecycleEvent{ch1, ch2} {
		select {
		case event := <-ch:
			assert.Equal(t, LifecycleEventStop, event.Action)
			require.NotNil(t, event.Instance)
			assert.Equal(t, StateStopped, event.Instance.State)
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for lifecycle event")
		}
	}
}

func TestLifecycleSubscribers_UnsubscribeStopsDelivery(t *testing.T) {
	s := newLifecycleSubscribers()
	ch, unsub := s.Subscribe(LifecycleEventConsumerWaitForState)
	unsub()

	_, ok := <-ch
	assert.False(t, ok, "channel should be closed after unsubscribe")
}

func TestLifecycleSubscribers_StatsByConsumer(t *testing.T) {
	s := newLifecycleSubscribers()
	wait1, unsub1 := s.Subscribe(LifecycleEventConsumerWaitForState)
	defer unsub1()
	wait2, unsub2 := s.Subscribe(LifecycleEventConsumerWaitForState)
	defer unsub2()
	auto, unsub3 := s.Subscribe(LifecycleEventConsumerAutoStandby)
	defer unsub3()

	for i := 0; i < 3; i++ {
		s.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventUpdate,
			InstanceID: "inst-1",
			Instance:   &Instance{State: StateRunning},
		})
	}
	<-wait1
	<-wait2

	stats := s.Stats()
	assert.Equal(t, int64(2), stats[LifecycleEventConsumerWaitForState].Subscribers)
	assert.Equal(t, int64(2), stats[LifecycleEventConsumerWaitForState].MaxQueueDepth)
	assert.Equal(t, int64(1), stats[LifecycleEventConsumerAutoStandby].Subscribers)
	assert.Equal(t, int64(3), stats[LifecycleEventConsumerAutoStandby].MaxQueueDepth)
	assert.Equal(t, 3, len(auto))
}

func TestLifecycleSubscribers_DropCallbackOnBackpressure(t *testing.T) {
	s := newLifecycleSubscribers()

	drops := make(chan LifecycleEventConsumer, 1)
	s.onDrop = func(ctx context.Context, consumer LifecycleEventConsumer) {
		drops <- consumer
	}

	_, unsub := s.Subscribe(LifecycleEventConsumerWaitForState)
	defer unsub()

	for i := 0; i < lifecycleEventBufferSize; i++ {
		s.Notify(context.Background(), LifecycleEvent{
			Action:     LifecycleEventUpdate,
			InstanceID: "inst-1",
			Instance:   &Instance{State: StateRunning},
		})
	}

	s.Notify(context.Background(), LifecycleEvent{
		Action:     LifecycleEventStart,
		InstanceID: "inst-1",
		Instance:   &Instance{State: StateRunning},
	})

	select {
	case consumer := <-drops:
		assert.Equal(t, LifecycleEventConsumerWaitForState, consumer)
	case <-time.After(time.Second):
		t.Fatal("expected drop callback")
	}
}
