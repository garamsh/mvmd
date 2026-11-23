package catalog

import (
	"context"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// HoldDiskSnapshot implements machine.Catalog.
func (s *service) HoldDiskSnapshot(snapshot machine.DiskSnapshotID, exclusive bool) func() {
	return s.diskSnapLocks.Lock(snapshot, exclusive)
}

// requireDiskSnapshots is the outcome of a call on a node that offers no disk
// snapshots.
func (s *service) requireDiskSnapshots() error {
	if s.diskSnaps == nil {
		return fail(machine.ErrUnsupported, "the node offers no disk snapshots")
	}
	return nil
}

func (s *service) ListDiskSnapshots(ctx context.Context) ([]machine.DiskSnapshotInfo, error) {
	if err := s.requireDiskSnapshots(); err != nil {
		return nil, err
	}
	all, err := s.diskSnaps.List(ctx)
	if err != nil {
		return nil, fail(machine.ErrFailed, "list the disk snapshots: %v", err)
	}
	return all, nil
}

func (s *service) DeleteDiskSnapshot(ctx context.Context, snapshot machine.DiskSnapshotID) error {
	if !machine.ValidDiskID(machine.DiskID(snapshot)) {
		return machine.InvalidDiskSnapshot(snapshot)
	}
	if err := s.requireDiskSnapshots(); err != nil {
		return err
	}
	// A call using the snapshot holds the lease; refuse rather than wait, since the
	// snapshot may be gone by then.
	unlock, ok := s.diskSnapLocks.TryLockExclusive(snapshot)
	if !ok {
		return fail(machine.ErrPreconditionFailed, "disk snapshot %q is in use by a snapshot or create disk call", snapshot)
	}
	defer unlock()
	ctx, cancel := s.accepted(ctx)
	defer cancel()
	if err := s.diskSnaps.Remove(ctx, snapshot); err != nil {
		return fail(machine.ErrFailed, "remove disk snapshot %q: %v", snapshot, err)
	}
	return nil
}
