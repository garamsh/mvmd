package memory

import (
	"context"
	"slices"
	"strings"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// DiskSnapshotStore is an in-memory machine.DiskSnapshotStore. Its steps are
// "put", "get", "list" and "remove". It holds the source path each snapshot was
// put from, and nothing is copied.
type DiskSnapshotStore struct {
	Failures

	mu     sync.Mutex
	held   map[machine.DiskSnapshotID]machine.DiskSnapshot
	source map[machine.DiskSnapshotID]string
	sizes  map[machine.DiskSnapshotID]uint64
}

// NewDiskSnapshotStore returns a DiskSnapshotStore that holds nothing.
func NewDiskSnapshotStore() *DiskSnapshotStore {
	return &DiskSnapshotStore{held: make(map[machine.DiskSnapshotID]machine.DiskSnapshot), source: make(map[machine.DiskSnapshotID]string), sizes: make(map[machine.DiskSnapshotID]uint64)}
}

// Put implements machine.DiskSnapshotStore.
func (s *DiskSnapshotStore) Put(ctx context.Context, snapshot machine.DiskSnapshot, source string) error {
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
		return machine.ErrDiskSnapshotExists
	}
	s.held[snapshot.ID] = snapshot
	s.source[snapshot.ID] = source
	return nil
}

// Get implements machine.DiskSnapshotStore. The path it returns is
// "/disk-snapshots/" and the identifier.
func (s *DiskSnapshotStore) Get(_ context.Context, id machine.DiskSnapshotID) (machine.DiskSnapshot, string, error) {
	if err := s.Enter("get"); err != nil {
		return machine.DiskSnapshot{}, "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.held[id]
	if !ok {
		return machine.DiskSnapshot{}, "", machine.ErrDiskSnapshotAbsent
	}
	return h, "/disk-snapshots/" + string(id), nil
}

// List implements machine.DiskSnapshotStore.
func (s *DiskSnapshotStore) List(_ context.Context) ([]machine.DiskSnapshotInfo, error) {
	if err := s.Enter("list"); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]machine.DiskSnapshotInfo, 0, len(s.held))
	for id, h := range s.held {
		out = append(out, machine.DiskSnapshotInfo{DiskSnapshot: h, AllocatedBytes: s.sizes[id]})
	}
	slices.SortFunc(out, func(a, b machine.DiskSnapshotInfo) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

// Remove implements machine.DiskSnapshotStore.
func (s *DiskSnapshotStore) Remove(_ context.Context, id machine.DiskSnapshotID) error {
	if err := s.Enter("remove"); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.held, id)
	delete(s.source, id)
	delete(s.sizes, id)
	return nil
}

// Provide makes the store hold a snapshot, as how one comes to exist is not
// the lifecycle's concern.
func (s *DiskSnapshotStore) Provide(snapshot machine.DiskSnapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[snapshot.ID] = snapshot
}

// Holds reports whether the store holds the snapshot.
func (s *DiskSnapshotStore) Holds(id machine.DiskSnapshotID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.held[id]
	return ok
}

// Source returns the path the snapshot was put from.
func (s *DiskSnapshotStore) Source(id machine.DiskSnapshotID) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.source[id]
	return p, ok
}
