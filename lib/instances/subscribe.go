package instances

import "sync"

// StateChange represents a state transition for an instance.
type StateChange struct {
	State      State
	StateError *string
}

// subscribers manages per-instance state change subscriptions.
type subscribers struct {
	mu   sync.Mutex
	subs map[string][]chan StateChange
}

func newSubscribers() *subscribers {
	return &subscribers{
		subs: make(map[string][]chan StateChange),
	}
}

// Subscribe returns a channel that receives state changes for the given
// instance and an unsubscribe function. The channel is buffered (16) to
// avoid blocking the notifier on slow consumers.
func (s *subscribers) Subscribe(instanceID string) (<-chan StateChange, func()) {
	ch := make(chan StateChange, 16)
	s.mu.Lock()
	s.subs[instanceID] = append(s.subs[instanceID], ch)
	s.mu.Unlock()

	return ch, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		chans := s.subs[instanceID]
		for i, c := range chans {
			if c == ch {
				s.subs[instanceID] = append(chans[:i], chans[i+1:]...)
				close(ch)
				break
			}
		}
		if len(s.subs[instanceID]) == 0 {
			delete(s.subs, instanceID)
		}
	}
}

// Notify fans out a state change to all subscribers for the given instance.
// Non-blocking: drops the event if a subscriber's buffer is full.
// Safe to call concurrently with CloseAll.
func (s *subscribers) Notify(instanceID string, sc StateChange) {
	s.mu.Lock()
	chans := make([]chan StateChange, len(s.subs[instanceID]))
	copy(chans, s.subs[instanceID])
	s.mu.Unlock()

	for _, ch := range chans {
		trySend(ch, sc)
	}
}

// trySend attempts a non-blocking send on ch, recovering from a panic if the
// channel was closed by a concurrent CloseAll.
func trySend(ch chan StateChange, sc StateChange) {
	defer func() { recover() }()
	select {
	case ch <- sc:
	default:
	}
}

// CloseAll closes and removes all subscriber channels for the given instance.
func (s *subscribers) CloseAll(instanceID string) {
	s.mu.Lock()
	chans := s.subs[instanceID]
	delete(s.subs, instanceID)
	s.mu.Unlock()

	for _, ch := range chans {
		close(ch)
	}
}
