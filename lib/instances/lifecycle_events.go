package instances

import (
	"context"
	"sync"
)

const defaultLifecycleEventBufferSize = 256

// LifecycleEventConsumer identifies the internal consumer of lifecycle events.
// Keep this set bounded for observability label safety.
type LifecycleEventConsumer string

const (
	LifecycleEventConsumerWaitForState LifecycleEventConsumer = "wait_for_state"
	LifecycleEventConsumerAutoStandby  LifecycleEventConsumer = "auto_standby"
)

var allLifecycleEventConsumers = []LifecycleEventConsumer{
	LifecycleEventConsumerWaitForState,
	LifecycleEventConsumerAutoStandby,
}

// LifecycleEventAction identifies which instance lifecycle action occurred.
type LifecycleEventAction string

const (
	LifecycleEventCreate  LifecycleEventAction = "create"
	LifecycleEventUpdate  LifecycleEventAction = "update"
	LifecycleEventStart   LifecycleEventAction = "start"
	LifecycleEventStop    LifecycleEventAction = "stop"
	LifecycleEventStandby LifecycleEventAction = "standby"
	LifecycleEventRestore LifecycleEventAction = "restore"
	LifecycleEventDelete  LifecycleEventAction = "delete"
	LifecycleEventFork    LifecycleEventAction = "fork"
)

// LifecycleEvent is a global instance change event stream used by internal
// consumers such as wait-for-state and background controllers.
type LifecycleEvent struct {
	Action     LifecycleEventAction
	InstanceID string
	Instance   *Instance
}

type lifecycleSubscriber struct {
	consumer LifecycleEventConsumer
	ch       chan LifecycleEvent
}

type lifecycleConsumerStats struct {
	Subscribers   int64
	MaxQueueDepth int64
}

type lifecycleSubscribers struct {
	mu         sync.Mutex
	subs       []lifecycleSubscriber
	onDrop     func(context.Context, LifecycleEventConsumer)
	bufferSize int
}

func newLifecycleSubscribers() *lifecycleSubscribers {
	return &lifecycleSubscribers{
		bufferSize: defaultLifecycleEventBufferSize,
	}
}

func newLifecycleSubscribersWithBufferSize(bufferSize int) *lifecycleSubscribers {
	if bufferSize <= 0 {
		bufferSize = defaultLifecycleEventBufferSize
	}
	return &lifecycleSubscribers{
		bufferSize: bufferSize,
	}
}

func (s *lifecycleSubscribers) Subscribe(consumer LifecycleEventConsumer) (<-chan LifecycleEvent, func()) {
	ch := make(chan LifecycleEvent, s.bufferSize)

	s.mu.Lock()
	s.subs = append(s.subs, lifecycleSubscriber{
		consumer: consumer,
		ch:       ch,
	})
	s.mu.Unlock()

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, sub := range s.subs {
			if sub.ch == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				close(ch)
				break
			}
		}
	}
}

func (s *lifecycleSubscribers) Notify(ctx context.Context, event LifecycleEvent) {
	s.mu.Lock()
	subs := append([]lifecycleSubscriber(nil), s.subs...)
	s.mu.Unlock()

	for _, sub := range subs {
		if trySendLifecycleEvent(sub.ch, event) && s.onDrop != nil {
			s.onDrop(ctx, sub.consumer)
		}
	}
}

func (s *lifecycleSubscribers) Stats() map[LifecycleEventConsumer]lifecycleConsumerStats {
	s.mu.Lock()
	defer s.mu.Unlock()

	stats := make(map[LifecycleEventConsumer]lifecycleConsumerStats, len(allLifecycleEventConsumers))
	for _, consumer := range allLifecycleEventConsumers {
		stats[consumer] = lifecycleConsumerStats{}
	}
	for _, sub := range s.subs {
		stat := stats[sub.consumer]
		stat.Subscribers++
		queueDepth := int64(len(sub.ch))
		if queueDepth > stat.MaxQueueDepth {
			stat.MaxQueueDepth = queueDepth
		}
		stats[sub.consumer] = stat
	}
	return stats
}

// trySendLifecycleEvent attempts a non-blocking send.
// Returns true when the event was dropped because the channel buffer was full.
func trySendLifecycleEvent(ch chan LifecycleEvent, event LifecycleEvent) (dropped bool) {
	defer func() {
		if recover() != nil {
			dropped = false
		}
	}()

	select {
	case ch <- event:
		return false
	default:
		return true
	}
}
