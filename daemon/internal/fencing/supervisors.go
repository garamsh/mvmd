package fencing

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Supervisor is a live supervisor and the expiry it holds.
type Supervisor struct {
	Machine machine.MachineID
	Expiry  BootTime
}

// Supervisors is the conversation with the per-Machine supervisors, as the
// fencing deadline needs it. A supervisor never holds an expiry later than the
// one it was last given, and no supervisor exists without one.
type Supervisors interface {
	// RunDir returns the Machine's run directory, which Start uses as the
	// description gives it.
	RunDir(id machine.MachineID) (string, error)
	// Start starts a supervisor for the Machine with the process to run, the
	// user to run it as and the
	// expiry it counts.
	Start(ctx context.Context, id machine.MachineID, process machine.ProcessDescription, user machine.User, expiry BootTime) error
	// SetExpiry passes the supervisor a new expiry, which replaces its own.
	SetExpiry(ctx context.Context, id machine.MachineID, expiry BootTime) error
	// Terminate kills the supervisor and with it the Machine's VMM, returning
	// once they have ended. It succeeds when none exists.
	Terminate(ctx context.Context, id machine.MachineID) error
	// Live lists the supervisors that exist with what each holds.
	Live(ctx context.Context) ([]Supervisor, error)
}
