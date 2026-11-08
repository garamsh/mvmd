// Package fencing is the node's fencing deadline in the inside of the mvmd
// daemon: renewal, inspection, the refusal of starts while the deadline is not
// armed, the forced stop of every Machine at expiry, and the reconciliation on
// start. It names no clock, process or file technology; every crossing is a
// port declared here.
package fencing

import "time"

// BootTime is a point on the host's boot clock, as an offset from boot. The
// clock counts suspension and cannot be set, so two values compare the same
// to mvmd and to a supervisor.
type BootTime time.Duration

// Add returns the point d after t.
func (t BootTime) Add(d time.Duration) BootTime { return t + BootTime(d) }

// Sub returns the time from u to t.
func (t BootTime) Sub(u BootTime) time.Duration { return time.Duration(t - u) }

// Record is the durable deadline: the expiry, the boot it was taken in, the
// length the client chose and the last renewal number applied.
type Record struct {
	Expiry  BootTime
	BootID  string
	Length  time.Duration
	Renewal uint64
}

// Phase is where the deadline is.
type Phase int

const (
	// PhaseUnarmed is a deadline no renewal has armed in this boot.
	PhaseUnarmed Phase = iota
	// PhaseArmed is an expiry in the future.
	PhaseArmed
	// PhaseExpired is an expiry that has passed and whose Machines are not yet
	// all stopped.
	PhaseExpired
	// PhaseStopped is an expiry that has passed after mvmd stopped everything.
	PhaseStopped
)

// String returns the phase's name.
func (p Phase) String() string {
	switch p {
	case PhaseUnarmed:
		return "unarmed"
	case PhaseArmed:
		return "armed"
	case PhaseExpired:
		return "expired"
	case PhaseStopped:
		return "stopped everything"
	default:
		return "unknown"
	}
}

// Status is what Inspect deadline reports.
type Status struct {
	Phase Phase
	// Length is the length the last renewal chose, zero while unarmed.
	Length time.Duration
	// Remaining is the time left, zero unless armed.
	Remaining time.Duration
	// Renewal is the last renewal number applied.
	Renewal uint64
}
