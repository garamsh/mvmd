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
	deadline := time.Now().Add(time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
