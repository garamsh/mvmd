package machine

import (
	"context"
	"time"
)

// Fences is the own fence of a Machine created with one (ADR 0064): the part
// of the fencing deadline the lifecycle speaks to. Renewal and inspection are
// the deadline's own calls and do not pass through the lifecycle.
type Fences interface {
	// Enrol makes the Machine's fence, not armed. It is called before the
	// Machine's record is written, and replaces anything a Machine of the
	// name left.
	Enrol(ctx context.Context, id MachineID) error
	// Release removes the Machine's fence. It succeeds when none exists.
	Release(ctx context.Context, id MachineID) error
	// Lapse says whether the Machine's own fence has lapsed in this boot, with
	// the node's deadline not before it, and how long ago. A VMM found ended
	// while it has is one the fence ended.
	Lapse(ctx context.Context, id MachineID) (ago time.Duration, lapsed bool, err error)
}

// WithFences gives the lifecycle the Machines' own fences. Without it a Machine
// cannot be created with one.
func WithFences(f Fences) Option {
	return func(s *service) { s.fences = f }
}

// reasonFenceLapsed begins the reason of a Machine whose own fence ended its
// VMM.
const reasonFenceLapsed = "the Machine's own fence lapsed and ended its VMM"

// fenceLapse returns the reason and the time of a Machine's VMM ended by its
// own fence, if it was. The time is the lapse, which came before this look.
func (s *service) fenceLapse(ctx context.Context, m Machine) (reason string, at time.Time, lapsed bool) {
	if !m.Fenced || s.fences == nil {
		return "", time.Time{}, false
	}
	ago, ok, err := s.fences.Lapse(ctx, m.ID)
	if err != nil || !ok {
		return "", time.Time{}, false
	}
	return reasonFenceLapsed + ": " + s.endCause(ctx, m.ID), s.clock.Now().Add(-ago), true
}
