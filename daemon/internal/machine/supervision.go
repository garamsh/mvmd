package machine

import (
	"context"
	"errors"
)

// ErrNotAttempted is what a port wraps when its call had no effect, so the
// caller knows the thing it asked for never began.
var ErrNotAttempted = errors.New("not attempted")

// Supervision is the conversation with the per-Machine supervisor that starts
// and confines the VMM process. The fencing expiry joins Start with the
// fencing issue.
type Supervision interface {
	// Start starts the described VMM process under a new supervisor, as the
	// Machine's user. The user comes from the lifecycle and never from the
	// Driver's description (ADR 0015).
	Start(ctx context.Context, id MachineID, process ProcessDescription, user User) error
	// Terminate forces the Machine's VMM process down and returns once it has
	// ended. It succeeds when no process exists. An error wrapping
	// ErrNotAttempted means nothing was done.
	Terminate(ctx context.Context, id MachineID) error
	// Live lists the Machines that have a supervisor now.
	Live(ctx context.Context) ([]MachineID, error)
}
