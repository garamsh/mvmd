package image

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Users hands each conversion its own user from the node's range of UIDs, the
// range the lifecycle allocates its Machines' users from (ADR 0015).
type Users interface {
	// Allocate returns a user no other Machine or conversion holds.
	Allocate(ctx context.Context) (machine.User, error)
	// Release returns the user once no VMM process of it exists.
	Release(user machine.User)
}
