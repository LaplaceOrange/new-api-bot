package bot

import "sync"

type keyedLockEntry struct {
	mu   sync.Mutex
	refs int
}

// keyedLocker serializes work for the same key and removes idle lock entries.
// Acquirers increment refs before waiting so an entry cannot be replaced while
// another goroutine is queued on it.
type keyedLocker[K comparable] struct {
	mu      sync.Mutex
	entries map[K]*keyedLockEntry
}

func (l *keyedLocker[K]) Lock(key K) func() {
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[K]*keyedLockEntry)
	}
	entry := l.entries[key]
	if entry == nil {
		entry = &keyedLockEntry{}
		l.entries[key] = entry
	}
	entry.refs++
	l.mu.Unlock()

	entry.mu.Lock()
	return func() {
		entry.mu.Unlock()
		l.mu.Lock()
		entry.refs--
		if entry.refs == 0 {
			delete(l.entries, key)
		}
		l.mu.Unlock()
	}
}

func (l *keyedLocker[K]) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}
