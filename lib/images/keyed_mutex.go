package images

import "sync"

type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*keyedLock
}

type keyedLock struct {
	mu   sync.Mutex
	refs int
}

func (m *keyedMutex) lock(key string) func() {
	m.mu.Lock()
	if m.locks == nil {
		m.locks = make(map[string]*keyedLock)
	}
	lock := m.locks[key]
	if lock == nil {
		lock = &keyedLock{}
		m.locks[key] = lock
	}
	lock.refs++
	m.mu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()

		m.mu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(m.locks, key)
		}
		m.mu.Unlock()
	}
}
