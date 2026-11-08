package memory

import (
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Events is an in-memory machine.Events that records what it was told.
type Events struct {
	mu       sync.Mutex
	changes  []Change
	reported []machine.DriverEvent
	deleted  []machine.MachineID
}

// Change is one state a Machine was reported to be in.
type Change struct {
	ID    machine.MachineID
	State machine.State
}

// NewEvents returns an Events that has recorded nothing.
func NewEvents() *Events { return &Events{} }

// StateChanged implements machine.Events.
func (e *Events) StateChanged(id machine.MachineID, state machine.State) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.changes = append(e.changes, Change{id, state})
}

// Deleted implements machine.Events.
func (e *Events) Deleted(id machine.MachineID) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.deleted = append(e.deleted, id)
}

// Deletions returns the Machines recorded as deleted, in order.
func (e *Events) Deletions() []machine.MachineID {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]machine.MachineID(nil), e.deleted...)
}

// Reported implements machine.Events.
func (e *Events) Reported(event machine.DriverEvent) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.reported = append(e.reported, event)
}

// Changes returns every state change recorded, in order.
func (e *Events) Changes() []Change {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Change(nil), e.changes...)
}

// States returns the states recorded for the Machine, in order.
func (e *Events) States(id machine.MachineID) []machine.State {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []machine.State
	for _, c := range e.changes {
		if c.ID == id {
			out = append(out, c.State)
		}
	}
	return out
}

// DriverEvents returns every Driver event recorded, in order.
func (e *Events) DriverEvents() []machine.DriverEvent {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]machine.DriverEvent(nil), e.reported...)
}
