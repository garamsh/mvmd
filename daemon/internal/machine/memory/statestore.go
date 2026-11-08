package memory

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// StateStore is an in-memory machine.StateStore. Its steps are "save" and
// "remove".
type StateStore struct {
	Failures

	mu      sync.Mutex
	records map[machine.MachineID]machine.Record
}

// NewStateStore returns an empty StateStore.
func NewStateStore() *StateStore {
	return &StateStore{records: make(map[machine.MachineID]machine.Record)}
}

// Save implements machine.StateStore.
func (s *StateStore) Save(_ context.Context, record machine.Record) error {
	if err := s.Enter("save"); err != nil {
		return err
	}
	s.mu.Lock()
	s.records[record.ID] = record
	s.mu.Unlock()
	return nil
}

// Remove implements machine.StateStore.
func (s *StateStore) Remove(_ context.Context, id machine.MachineID) error {
	if err := s.Enter("remove"); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.records, id)
	s.mu.Unlock()
	return nil
}

// Record returns what is durably recorded for the identifier.
func (s *StateStore) Record(id machine.MachineID) (machine.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	return r, ok
}
