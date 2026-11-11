package memory

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// SnapshotStore is an in-memory machine.SnapshotStore. Its steps are "put",
// "get", "list" and "remove". It holds the files by name only: nothing is copied.
type SnapshotStore struct {
	Failures

	mu    sync.Mutex
	held  map[machine.SnapshotID]heldSnapshot
	sizes map[machine.SnapshotID]uint64
	order []machine.SnapshotID
	// offered is the Driver files of every Put asked for.
	offered [][]string
}

type heldSnapshot struct {
	snapshot machine.Snapshot
	files    machine.SnapshotFiles
}

// NewSnapshotStore returns a SnapshotStore that holds nothing.
func NewSnapshotStore() *SnapshotStore {
	return &SnapshotStore{held: make(map[machine.SnapshotID]heldSnapshot), sizes: make(map[machine.SnapshotID]uint64)}
}

// Put implements machine.SnapshotStore.
func (s *SnapshotStore) Put(ctx context.Context, snapshot machine.Snapshot, files machine.SnapshotFiles) error {
	s.mu.Lock()
	s.offered = append(s.offered, slices.Clone(files.State))
	s.mu.Unlock()
	if err := s.Enter("put"); err != nil {
		return err
	}
	// A Put whose context has ended by the time it is made stores nothing.
	if err := ctx.Err(); err != nil {
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

// List implements machine.SnapshotStore.
func (s *SnapshotStore) List(_ context.Context) ([]machine.SnapshotInfo, error) {
	if err := s.Enter("list"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]machine.SnapshotInfo, 0, len(s.held))
	for id, h := range s.held {
		out = append(out, machine.SnapshotInfo{Snapshot: h.snapshot, SizeBytes: s.sizes[id]})
	}
	slices.SortFunc(out, func(a, b machine.SnapshotInfo) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

// SetSize sets the size List reports for a snapshot the store holds. A
// snapshot SetSize was not called for reports a size of zero.
func (s *SnapshotStore) SetSize(id machine.SnapshotID, sizeBytes uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.held[id]; ok {
		s.sizes[id] = sizeBytes
	}
}

// Remove implements machine.SnapshotStore.
func (s *SnapshotStore) Remove(_ context.Context, id machine.SnapshotID) error {
	if err := s.Enter("remove"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, id)
	delete(s.sizes, id)
	s.order = slices.DeleteFunc(s.order, func(o machine.SnapshotID) bool { return o == id })
	return nil
}

// Offered returns the Driver files of each Put the store was asked for, kept
// or not.
func (s *SnapshotStore) Offered() [][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.offered)
}

// Held returns the identifiers of the snapshots held, in the order they were
// put.
func (s *SnapshotStore) Held() []machine.SnapshotID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.order)
}
