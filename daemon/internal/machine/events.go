package machine

// DriverEvent is something a Driver reports on its own, outside any call the
// lifecycle made, such as a device that failed. A VMM that ended is not one:
// the lifecycle finds it through the supervisors it lists.
type DriverEvent struct {
	Name   string
	Detail string
}

// Events is the lifecycle's record of what happened to its Machines. The
// lifecycle calls it from the one place a Machine's state is committed, so no
// path can change a state without it, from the one place a Delete commits,
// and for every event a Driver reports.
type Events interface {
	// StateChanged records that the Machine's state is now state.
	StateChanged(id MachineID, state State)
	// Deleted records that the Machine no longer exists. A deleted Machine
	// has no state, so this is not a StateChanged.
	Deleted(id MachineID)
	// Reported records an event a Driver raised.
	Reported(event DriverEvent)
}
