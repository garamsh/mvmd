package memory

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// FenceStore is an in-memory fencing.FenceStore. Its steps are "load", "save"
// and "delete".
type FenceStore struct {
	machinememory.Failures

	mu   sync.Mutex
	recs map[machine.MachineID]fencing.Record
}

// NewFenceStore returns a store with no record.
func NewFenceStore() *FenceStore {
	return &FenceStore{recs: make(map[machine.MachineID]fencing.Record)}
}

// Load implements fencing.FenceStore.
func (s *FenceStore) Load(context.Context) (map[machine.MachineID]fencing.Record, error) {
	if err := s.Enter("load"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[machine.MachineID]fencing.Record, len(s.recs))
	for id, r := range s.recs {
		out[id] = r
	}
	return out, nil
}

// Save implements fencing.FenceStore.
func (s *FenceStore) Save(_ context.Context, id machine.MachineID, record fencing.Record) error {
	if err := s.Enter("save"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.recs[id] = record
	return nil
}

// Delete implements fencing.FenceStore.
func (s *FenceStore) Delete(_ context.Context, id machine.MachineID) error {
	if err := s.Enter("delete"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.recs, id)
	return nil
}

// Recorded returns the Machine's record, if one was saved.
func (s *FenceStore) Recorded(id machine.MachineID) (fencing.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.recs[id]
	return r, ok
}
