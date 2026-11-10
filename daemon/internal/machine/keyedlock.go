package machine

import "sync"

// keyedLock is one key's lock and the number of calls using it, which the
// owning keyedLocks guards.
type keyedLock struct {
	sync.RWMutex
	users int
}

// keyedLocks is a lock per key, shared or exclusive. A key's entry exists
// while a call holds or waits for it and is removed by the last one, so the
// map holds only the keys in use now. The zero value is ready to use.
type keyedLocks[K comparable] struct {
	mu sync.Mutex
	m  map[K]*keyedLock
}

// lock takes the key's lock, exclusively or shared, and returns the unlock.
func (k *keyedLocks[K]) lock(key K, exclusive bool) func() {
	l, leave := k.enter(key)
	lock, unlock := l.RLock, l.RUnlock
	if exclusive {
		lock, unlock = l.Lock, l.Unlock
	}
	lock()
	return func() {
		unlock()
		leave()
	}
}

// tryLockExclusive takes the key's lock exclusively only if no call holds or
// waits on it, and returns the unlock, or false holding nothing.
func (k *keyedLocks[K]) tryLockExclusive(key K) (func(), bool) {
	l, leave := k.enter(key)
	if !l.TryLock() {
		leave()
		return nil, false
	}
	return func() {
		l.Unlock()
		leave()
	}, true
}

// enter counts a user of the key's lock, creating the lock for the first, and
// returns it with the function that stops counting.
func (k *keyedLocks[K]) enter(key K) (*keyedLock, func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = make(map[K]*keyedLock)
	}
	l, ok := k.m[key]
	if !ok {
		l = new(keyedLock)
		k.m[key] = l
	}
	l.users++
	return l, func() {
		k.mu.Lock()
		defer k.mu.Unlock()
		if l.users--; l.users == 0 {
			delete(k.m, key)
		}
	}
}

// held returns how many keys have a lock now.
func (k *keyedLocks[K]) held() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}
