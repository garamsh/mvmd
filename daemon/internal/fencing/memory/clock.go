// Package memory holds the in-memory substitutes for the ports of package
// fencing. They serve tests and development; the store and the supervisors can
// be told to fail at a named step, and together they record any moment a
// supervisor held an expiry later than the recorded one.
package memory

import (
	"context"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
)

// Clock is an in-memory fencing.Clock that moves only when told to.
type Clock struct {
	mu      sync.Mutex
	now     fencing.BootTime
	bootID  string
	waiters []*waiter
}

type waiter struct {
	until fencing.BootTime
	done  chan struct{}
}

// NewClock returns a clock at boot time one hour, of the given boot.
func NewClock(bootID string) *Clock {
	return &Clock{now: fencing.BootTime(time.Hour), bootID: bootID}
}

// Now implements fencing.Clock.
func (c *Clock) Now() fencing.BootTime {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// BootID implements fencing.Clock.
func (c *Clock) BootID() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bootID
}

// Wait implements fencing.Clock.
func (c *Clock) Wait(ctx context.Context, until fencing.BootTime) error {
	c.mu.Lock()
	if c.now >= until {
		c.mu.Unlock()
		return nil
	}
	w := &waiter{until: until, done: make(chan struct{})}
	c.waiters = append(c.waiters, w)
	c.mu.Unlock()

	select {
	case <-w.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Advance moves the clock forward, in one step however long, as a suspended
// host does, and wakes the waits it reaches.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	kept := c.waiters[:0]
	for _, w := range c.waiters {
		if c.now >= w.until {
			close(w.done)
		} else {
			kept = append(kept, w)
		}
	}
	c.waiters = kept
}

// Reboot starts a new boot: a new identity and a clock from zero.
func (c *Clock) Reboot(bootID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now, c.bootID = 0, bootID
}
