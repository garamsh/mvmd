// Package keyedlock is a lock per key, shared or exclusive, for the modules that
// serialise calls on a name: the lifecycle and the catalog of the resources a
// Machine names share this one implementation and none of its locks.
package keyedlock

import "sync"

// lockEntry is one key's lock and the number of calls using it, which the
// owning Locks guards.
type lockEntry struct {
	sync.RWMutex
	users int
}

// Locks is a lock per key, shared or exclusive. A key's entry exists
// while a call holds or waits for it and is removed by the last one, so the
// map holds only the keys in use now. The zero value is ready to use.
type Locks[K comparable] struct {
	mu sync.Mutex
	m  map[K]*lockEntry
}

// Lock takes the key's lock, exclusively or shared, and returns the unlock.
func (k *Locks[K]) Lock(key K, exclusive bool) func() {
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

// TryLockExclusive takes the key's lock exclusively only if no call holds or
// waits on it, and returns the unlock, or false holding nothing.
func (k *Locks[K]) TryLockExclusive(key K) (func(), bool) {
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
func (k *Locks[K]) enter(key K) (*lockEntry, func()) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.m == nil {
		k.m = make(map[K]*lockEntry)
	}
	l, ok := k.m[key]
	if !ok {
		l = new(lockEntry)
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

// Held returns how many keys have a lock now.
func (k *Locks[K]) Held() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.m)
}

// Users returns how many calls hold or wait for the key's lock.
func (k *Locks[K]) Users(key K) int {
	k.mu.Lock()
	defer k.mu.Unlock()
	if l, ok := k.m[key]; ok {
		return l.users
	}
	return 0
}
