package fencing

import "context"

// Clock is the host's boot clock and the identity of the boot it counts.
type Clock interface {
	// Now returns the boot time now.
	Now() BootTime
	// BootID identifies the host's current boot; a different one means a reboot.
	BootID() string
	// Wait returns nil once boot time has reached until, or the context's error.
	Wait(ctx context.Context, until BootTime) error
}
