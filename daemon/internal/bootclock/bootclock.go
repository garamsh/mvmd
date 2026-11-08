// Package bootclock is the host's CLOCK_BOOTTIME and boot identity as
// fencing.Clock. CLOCK_BOOTTIME counts suspension and cannot be set, which a
// deadline needs; the wall clock is never read (ADR 0003).
package bootclock

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
)

const bootIDPath = "/proc/sys/kernel/random/boot_id"

// Clock is the host's boot clock.
type Clock struct {
	bootID string
}

// New reads the identity of the host's current boot.
func New() (*Clock, error) {
	raw, err := os.ReadFile(bootIDPath)
	if err != nil {
		return nil, fmt.Errorf("read the boot identity: %w", err)
	}
	id := strings.TrimSpace(string(raw))
	if id == "" {
		return nil, fmt.Errorf("%s is empty", bootIDPath)
	}
	return &Clock{bootID: id}, nil
}

// Now implements fencing.Clock.
func (*Clock) Now() fencing.BootTime {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		// CLOCK_BOOTTIME exists since Linux 2.6.39 and the call has no failing
		// input; a failure is a broken invariant, not an answer.
		panic(fmt.Sprintf("clock_gettime(CLOCK_BOOTTIME): %v", err))
	}
	return fencing.BootTime(time.Duration(ts.Nano()))
}

// BootID implements fencing.Clock.
func (c *Clock) BootID() string { return c.bootID }

// Wait implements fencing.Clock. The timer counts the process's own clock, so
// it is re-read against the boot clock when it fires: a suspension that
// outlasted the timer is seen, and one that cut it short is waited out.
func (c *Clock) Wait(ctx context.Context, until fencing.BootTime) error {
	for {
		left := until.Sub(c.Now())
		if left <= 0 {
			return nil
		}
		timer := time.NewTimer(left)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		}
	}
}
