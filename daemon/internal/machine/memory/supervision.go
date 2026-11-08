package memory

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Supervision is an in-memory machine.Supervision. Its steps are "start" and
// "terminate".
type Supervision struct {
	Failures

	mu      sync.Mutex
	running map[machine.MachineID]machine.ProcessDescription
}

// NewSupervision returns a Supervision with no process running.
func NewSupervision() *Supervision {
	return &Supervision{running: make(map[machine.MachineID]machine.ProcessDescription)}
}

// Start implements machine.Supervision.
func (s *Supervision) Start(_ context.Context, id machine.MachineID, process machine.ProcessDescription) error {
	if err := s.Enter("start"); err != nil {
		return err
	}
	s.mu.Lock()
	s.running[id] = process
	s.mu.Unlock()
	return nil
}

// Terminate implements machine.Supervision.
func (s *Supervision) Terminate(_ context.Context, id machine.MachineID) error {
	if err := s.Enter("terminate"); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.running, id)
	s.mu.Unlock()
	return nil
}

// Running returns the process the Machine's supervisor was started with, and
// whether that process exists now.
func (s *Supervision) Running(id machine.MachineID) (machine.ProcessDescription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.running[id]
	return p, ok
}
