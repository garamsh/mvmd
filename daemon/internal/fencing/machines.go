package fencing

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Machines is the lifecycle as the deadline needs it: the Machines to bring
// to Stopped once their supervisors are gone. machine.Service satisfies it.
type Machines interface {
	List(ctx context.Context) ([]machine.Machine, error)
	// ForceStop ends the Machine's VMM at once and leaves it Stopped.
	ForceStop(ctx context.Context, id machine.MachineID) error
}
