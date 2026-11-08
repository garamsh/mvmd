package memory

import (
	"context"
	"slices"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// SnapshotStore is an in-memory machine.SnapshotStore. Its steps are "put",
// "get" and "remove". It holds the files by name only: nothing is copied.
type SnapshotStore struct {
	Failures

	mu    sync.Mutex
	held  map[machine.SnapshotID]heldSnapshot
	order []machine.SnapshotID
}

type heldSnapshot struct {
	snapshot machine.Snapshot
	files    machine.SnapshotFiles
}

// NewSnapshotStore returns a SnapshotStore that holds nothing.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{held: make(map[machine.SnapshotID]heldSnapshot)}
}

// Put implements machine.SnapshotStore.
func (s *SnapshotStore) Put(_ context.Context, snapshot machine.Snapshot, files machine.SnapshotFiles) error {
	if err := s.Enter("put"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[snapshot.ID]; ok {
		return machine.ErrSnapshotExists
	}
	files.State = slices.Clone(files.State)
	s.held[snapshot.ID] = heldSnapshot{snapshot, files}
	s.order = append(s.order, snapshot.ID)
	return nil
}

// Get implements machine.SnapshotStore.
func (s *SnapshotStore) Get(_ context.Context, id machine.SnapshotID) (machine.Snapshot, machine.SnapshotFiles, error) {
	if err := s.Enter("get"); err != nil {
		return machine.Snapshot{}, machine.SnapshotFiles{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.held[id]
	if !ok {
		return machine.Snapshot{}, machine.SnapshotFiles{}, machine.ErrSnapshotAbsent
	}
	h.files.State = slices.Clone(h.files.State)
	return h.snapshot, h.files, nil
}

// Remove implements machine.SnapshotStore.
func (s *SnapshotStore) Remove(_ context.Context, id machine.SnapshotID) error {
	if err := s.Enter("remove"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, id)
	s.order = slices.DeleteFunc(s.order, func(o machine.SnapshotID) bool { return o == id })
	return nil
}

// Held returns the identifiers of the snapshots held, in the order they were
// put.
func (s *SnapshotStore) Held() []machine.SnapshotID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}
