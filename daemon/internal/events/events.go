// Package events is the node's event log in the inside of the mvmd daemon: a
// numbered sequence of the state changes, deadline events and Driver events of
// this node, held for a bounded number of events and read by a watcher that
// may resume after a sequence number. It names no wire format; the driving
// adapter converts what it reads.
package events

import (
	"errors"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ErrResync is what a watcher gets when the events after the sequence it
// resumes from are no longer held, or were never held by this node. The
// client lists to resynchronise.
var ErrResync = errors.New("the events after the sequence are not held")

// Event is one entry of the log. Sequence grows by one per event on this
// node, from one.
type Event struct {
	Sequence uint64
	// Machine is empty for an event of the node, not of a Machine.
	Machine machine.MachineID
	Body    Body
}

// Body is what an event says: one of StateChanged, Deleted, Deadline,
// Driver or ProbeChanged.
type Body interface{ body() }

// StateChanged says the Machine's state is now State.
type StateChanged struct{ State machine.State }

// Deleted says the Machine no longer exists.
type Deleted struct{}

// Deadline says the fencing deadline changed.
type Deadline struct{ Event fencing.Event }

// Driver says a Driver raised an event.
type Driver struct{ Event machine.DriverEvent }

// ProbeChanged says a probe of the Machine has the Result, which differs from
// the one the node held.
type ProbeChanged struct{ Result machine.ProbeResult }

func (StateChanged) body() {}
func (ProbeChanged) body() {}
func (Deleted) body()      {}
func (Deadline) body()     {}
func (Driver) body()       {}
