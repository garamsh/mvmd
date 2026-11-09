package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// StateStore is an in-memory machine.StateStore. Its steps are "save",
// "save-exec", "remove" and "load".
type StateStore struct {
	Failures

	mu      sync.Mutex
	records map[machine.MachineID]machine.Record
	execs   map[machine.MachineID]map[machine.ExecID]machine.ExecEntry
}

// NewStateStore returns an empty StateStore.
func NewStateStore() *StateStore {
	return &StateStore{
		records: make(map[machine.MachineID]machine.Record),
		execs:   make(map[machine.MachineID]map[machine.ExecID]machine.ExecEntry),
	}
}

// Save implements machine.StateStore.
func (s *StateStore) Save(_ context.Context, record machine.Record) error {
	if err := s.Enter("save"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, e := range record.Execs {
		s.putExec(record.ID, e)
	}
	record.Execs = nil
	s.records[record.ID] = record
	return nil
}

// SaveExec implements machine.StateStore.
func (s *StateStore) SaveExec(_ context.Context, entry machine.ExecEntry) error {
	if err := s.Enter("save-exec"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.records[entry.Machine]; !ok {
		return fmt.Errorf("no record of Machine %q", entry.Machine)
	}
	s.putExec(entry.Machine, entry)
	return nil
}

// putExec stores the entry; the caller holds mu.
func (s *StateStore) putExec(id machine.MachineID, e machine.ExecEntry) {
	if s.execs[id] == nil {
		s.execs[id] = make(map[machine.ExecID]machine.ExecEntry)
	}
	s.execs[id][e.ID] = e
}

// Remove implements machine.StateStore.
func (s *StateStore) Remove(_ context.Context, id machine.MachineID) error {
	if err := s.Enter("remove"); err != nil {
		return err
	}
	s.mu.Lock()
	delete(s.records, id)
	delete(s.execs, id)
	s.mu.Unlock()
	return nil
}

// Load implements machine.StateStore.
func (s *StateStore) Load(context.Context) ([]machine.Record, error) {
	if err := s.Enter("load"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]machine.Record, 0, len(s.records))
	for id, r := range s.records {
		r.Execs = s.execsOf(id)
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b machine.Record) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

// execsOf returns the Machine's entries ordered by exec identifier; the caller
// holds mu.
func (s *StateStore) execsOf(id machine.MachineID) []machine.ExecEntry {
	var out []machine.ExecEntry
	for _, e := range s.execs[id] {
		out = append(out, e)
	}
	slices.SortFunc(out, func(a, b machine.ExecEntry) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out
}

// Record returns what is durably recorded for the identifier.
func (s *StateStore) Record(id machine.MachineID) (machine.Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[id]
	if ok {
		r.Execs = s.execsOf(id)
	}
	return r, ok
}
