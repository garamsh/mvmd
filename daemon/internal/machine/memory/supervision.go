package memory

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Supervision is an in-memory machine.Supervision. Its steps are "run-dir",
// "start", "terminate" and "live".
type Supervision struct {
	Failures

	mu      sync.Mutex
	running map[machine.MachineID]started
}

type started struct {
	process machine.ProcessDescription
	user    machine.User
}

// NewSupervision returns a Supervision with no process running.
func NewSupervision() *Supervision {
	return &Supervision{running: make(map[machine.MachineID]started)}
}

// Start implements machine.Supervision.
func (s *Supervision) Start(_ context.Context, id machine.MachineID, process machine.ProcessDescription, user machine.User) error {
	if err := s.Enter("start"); err != nil {
		return err
	}
	s.mu.Lock()
	s.running[id] = started{process, user}
	s.mu.Unlock()
	return nil
}

// RunDir implements machine.Supervision: a path of its own for each Machine.
func (s *Supervision) RunDir(id machine.MachineID) (string, error) {
	if err := s.Enter("run-dir"); err != nil {
		return "", err
	}
	return "/run/memory-supervision/" + string(id), nil
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

// Live implements machine.Supervision.
func (s *Supervision) Live(context.Context) ([]machine.MachineID, error) {
	if err := s.Enter("live"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []machine.MachineID
	for id := range s.running {
		out = append(out, id)
	}
	return out, nil
}

// Running returns the process the Machine's supervisor was started with, and
// whether that process exists now.
func (s *Supervision) Running(id machine.MachineID) (machine.ProcessDescription, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.running[id]
	return r.process, ok
}

// User returns the user the Machine's supervisor was started with.
func (s *Supervision) User(id machine.MachineID) (machine.User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.running[id]
	return r.user, ok
}
