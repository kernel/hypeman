package instances

import "sync"

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

// LifecycleEvent is a global instance change event stream used by background
// controllers that need to react to instance eligibility or identity changes.
type LifecycleEvent struct {
	Action     LifecycleEventAction
	InstanceID string
	Instance   *Instance
}

type lifecycleSubscribers struct {
	mu   sync.Mutex
	subs []chan LifecycleEvent
}

func newLifecycleSubscribers() *lifecycleSubscribers {
	return &lifecycleSubscribers{}
}

func (s *lifecycleSubscribers) Subscribe() (<-chan LifecycleEvent, func()) {
	ch := make(chan LifecycleEvent, 32)

	s.mu.Lock()
	s.subs = append(s.subs, ch)
	s.mu.Unlock()

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		for i, sub := range s.subs {
			if sub == ch {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				close(ch)
				break
			}
		}
	}
}

func (s *lifecycleSubscribers) Notify(event LifecycleEvent) {
	s.mu.Lock()
	subs := append([]chan LifecycleEvent(nil), s.subs...)
	s.mu.Unlock()

	for _, ch := range subs {
		trySendLifecycleEvent(ch, event)
	}
}

func trySendLifecycleEvent(ch chan LifecycleEvent, event LifecycleEvent) {
	defer func() { recover() }()

	select {
	case ch <- event:
	default:
	}
}
