package machine

import "context"

// StartPermitter says whether a Machine may be started now. The fencing issue
// fills it with the deadline; Start asks it before doing anything else.
type StartPermitter interface {
	// PermitStart returns nil to permit the start, or the reason it is refused.
	PermitStart(ctx context.Context, id MachineID) error
}
