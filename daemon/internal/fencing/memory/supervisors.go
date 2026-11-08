package memory

import (
	"context"
	"fmt"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// Supervisors is an in-memory fencing.Supervisors. Its steps are "start",
// "set-expiry", "terminate" and "live"; "set-expiry:<id>" and "terminate:<id>"
// name one Machine's supervisor.
type Supervisors struct {
	machinememory.Failures

	mu    sync.Mutex
	store *DeadlineStore
	live  map[machine.MachineID]held
}

type held struct {
	expiry  fencing.BootTime
	process machine.ProcessDescription
}

// NewSupervisors returns Supervisors with none live, checked against the
// store: any expiry a supervisor takes that is later than the recorded one is
// a violation of the store. With a nil store nothing is checked.
func NewSupervisors(store *DeadlineStore) *Supervisors {
	s := &Supervisors{store: store, live: make(map[machine.MachineID]held)}
	if store != nil {
		store.mu.Lock()
		store.supervisor = s
		store.mu.Unlock()
	}
	return s
}

// Start implements fencing.Supervisors.
func (s *Supervisors) Start(_ context.Context, id machine.MachineID, process machine.ProcessDescription, expiry fencing.BootTime) error {
	if err := s.Enter("start"); err != nil {
		return err
	}
	s.check("start of "+string(id), expiry)
	s.mu.Lock()
	s.live[id] = held{expiry: expiry, process: process}
	s.mu.Unlock()
	return nil
}

// SetExpiry implements fencing.Supervisors.
func (s *Supervisors) SetExpiry(_ context.Context, id machine.MachineID, expiry fencing.BootTime) error {
	err := s.Enter("set-expiry")
	if one := s.Enter("set-expiry:" + string(id)); err == nil {
		err = one
	}
	if err != nil {
		return err
	}
	s.check("expiry passed to "+string(id), expiry)
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.live[id]
	if !ok {
		return fmt.Errorf("no supervisor for %q", id)
	}
	h.expiry = expiry
	s.live[id] = h
	return nil
}

// Terminate implements fencing.Supervisors.
func (s *Supervisors) Terminate(_ context.Context, id machine.MachineID) error {
	err := s.Enter("terminate")
	if one := s.Enter("terminate:" + string(id)); err == nil {
		err = one
	}
	if err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.live, id)
	s.mu.Unlock()
	return nil
}

// Live implements fencing.Supervisors.
func (s *Supervisors) Live(context.Context) ([]fencing.Supervisor, error) {
	if err := s.Enter("live"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []fencing.Supervisor
	for id, h := range s.live {
		out = append(out, fencing.Supervisor{Machine: id, Expiry: h.expiry})
	}
	return out, nil
}

// Expiry returns what the Machine's supervisor holds, and whether one exists.
func (s *Supervisors) Expiry(id machine.MachineID) (fencing.BootTime, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.live[id]
	return h.expiry, ok
}

// Process returns the process the Machine's supervisor was started with.
func (s *Supervisors) Process(id machine.MachineID) (machine.ProcessDescription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.live[id]
	return h.process, ok
}

func (s *Supervisors) held() map[machine.MachineID]fencing.BootTime {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[machine.MachineID]fencing.BootTime, len(s.live))
	for id, h := range s.live {
		out[id] = h.expiry
	}
	return out
}

// check records a violation if the expiry is later than the recorded one, or
// if nothing is recorded.
func (s *Supervisors) check(what string, expiry fencing.BootTime) {
	if s.store == nil {
		return
	}
	rec, ok := s.store.Recorded()
	switch {
	case !ok:
		s.store.violate(what + ": no expiry is recorded")
	case expiry > rec.Expiry:
		s.store.violate(fmt.Sprintf("%s: %v is later than the recorded %v", what, expiry, rec.Expiry))
	}
}
