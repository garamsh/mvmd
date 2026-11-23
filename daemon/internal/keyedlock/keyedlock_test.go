package keyedlock_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/garamsh/mvmd/daemon/internal/keyedlock"
)

func TestLocks_SharedHoldersTogetherAndAnExclusiveOneAlone(t *testing.T) {
	var l keyedlock.Locks[string]

	first, second := l.Lock("k", false), l.Lock("k", false)
	_, ok := l.TryLockExclusive("k")
	assert.False(t, ok, "an exclusive try is refused beside shared holders")
	first()
	_, ok = l.TryLockExclusive("k")
	assert.False(t, ok, "and beside the one that is left")
	second()

	unlock, ok := l.TryLockExclusive("k")
	assert.True(t, ok, "control: with no holder it is taken")
	_, ok = l.TryLockExclusive("k")
	assert.False(t, ok, "and refused to a second")
	other, ok := l.TryLockExclusive("other")
	assert.True(t, ok, "a different key is its own lock")
	other()
	unlock()
}

func TestLocks_HoldAKeyOnlyWhileInUse(t *testing.T) {
	var l keyedlock.Locks[string]
	unlock := l.Lock("a", true)
	assert.Equal(t, 1, l.Held())
	unlock()
	assert.Zero(t, l.Held(), "the last user removes the key")
}
