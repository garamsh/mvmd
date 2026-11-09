package hostnet_test

import (
	"time"

	"github.com/garamsh/mvmd/daemon/internal/nstest"
)

// inNamespace runs a test that changes the kernel's network configuration in a
// network namespace made for it (internal/nstest).
var inNamespace = nstest.Run

// eventually polls cond for up to a second; the kernel hands a frame to the
// reader a moment after the write returns.
func eventually(cond func() bool) bool {
	return eventuallyWithin(time.Second, cond)
}

// eventuallyWithin polls cond for up to d, for a condition that depends on the
// kernel finishing work of its own, which a loaded machine delays.
func eventuallyWithin(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
