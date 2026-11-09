package hostnet_test

import (
	"time"

	"github.com/garamsh/mvmd/daemon/internal/nstest"
)

// inNamespace runs a test that changes the kernel's network configuration in a
// network namespace made for it (internal/nstest).
var inNamespace = nstest.Run

// eventually polls cond until it returns true or timeout elapses.
func eventually(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
