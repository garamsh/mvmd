package memory

import (
	"sync"
	"time"
)

// WallClock is an in-memory machine.WallClock that tells the time it was set to.
type WallClock struct {
	mu  sync.Mutex
	now time.Time
}

// NewWallClock returns a WallClock at the given time.
func NewWallClock(now time.Time) *WallClock { return &WallClock{now: now} }

// Now implements machine.WallClock.
func (c *WallClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Set moves the clock to the time.
func (c *WallClock) Set(now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
}
