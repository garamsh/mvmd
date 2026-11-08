package machine

// DriverEvent is something a Driver reports on its own, outside any call the
// lifecycle made: a VMM that exited, a device that failed.
type DriverEvent struct {
	Name   string
	Detail string
}

// Events is the lifecycle's record of what happened to its Machines. The
// lifecycle calls it from the one place a Machine's state is committed, so no
// path can change a state without it, and for every event a Driver reports.
type Events interface {
	// StateChanged records that the Machine's state is now state.
	StateChanged(id MachineID, state State)
	// Reported records an event a Driver raised.
	Reported(event DriverEvent)
}
