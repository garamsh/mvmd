package machine

import (
	"context"
	"errors"
	"fmt"
)

// invalidDiskSnapshot is the outcome of a call that names a disk snapshot
// identifier no file can have; it obeys the rules of a disk identifier.
func invalidDiskSnapshot(snapshot DiskSnapshotID) error {
	return fail(ErrInvalid, "%q is not a disk snapshot identifier: it must obey the rules of a disk identifier (must not be empty, ., .., start with %s, contain / or a NUL, or exceed %d bytes)", snapshot, TempDiskPrefix, maxNameBytes)
}

// requireDiskSnapshots is the outcome of a call on a node that offers no disk
// snapshots.
func (s *service) requireDiskSnapshots() error {
	if s.diskSnaps == nil {
		return fail(ErrUnsupported, "the node offers no disk snapshots")
	}
	return nil
}

func (s *service) SnapshotDisk(ctx context.Context, disk DiskID, snapshot DiskSnapshotID, request RequestID) error {
	switch {
	case !ValidDiskID(disk):
		return invalidDisk(disk)
	case !ValidDiskID(DiskID(snapshot)):
		return invalidDiskSnapshot(snapshot)
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	}
	if err := s.requireDiskSnapshots(); err != nil {
		return err
	}
	// No Create disk call reads the snapshot until this call has finished with
	// it, and none sees one that fails half way.
	defer s.diskSnapLocks.lock(snapshot, true)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	switch held, _, err := s.diskSnaps.Get(ctx, snapshot); {
	case err == nil:
		if held.Request == request && held.Disk == disk {
			return nil
		}
		return fail(ErrConflict, "disk snapshot %q exists under another request ID or another disk", snapshot)
	case !errors.Is(err, ErrDiskSnapshotAbsent):
		return fail(ErrFailed, "look up disk snapshot %q: %v", snapshot, err)
	}
	for attempt := 0; attempt < resizeAttempts; attempt++ {
		if done, err := s.snapshotDisk(ctx, disk, snapshot, request); done {
			return err
		}
	}
	return fail(ErrPreconditionFailed, "the holder of disk %q kept changing", disk)
}

// snapshotDisk is one attempt of SnapshotDisk. It reports done false when the
// holder changed under it and the call should look again.
func (s *service) snapshotDisk(ctx context.Context, disk DiskID, snapshot DiskSnapshotID, request RequestID) (done bool, _ error) {
	s.mu.Lock()
	holder, held := s.held[disk]
	s.mu.Unlock()
	if !held {
		// The holder slot keeps an attach from taking the disk, and a resize or
		// a delete from changing it, while it is copied.
		if _, _, ok := s.claim(snapshotting, disk); !ok {
			return false, nil
		}
		defer s.release(disk)
		return true, s.putDiskSnapshot(ctx, disk, snapshot, request)
	}
	if holder == deleting || holder == resizing || holder == snapshotting {
		return true, fail(ErrPreconditionFailed, "disk %q is %s", disk, holderIs(holder))
	}
	defer s.lock(holder)()
	s.mu.Lock()
	still := s.held[disk] == holder
	s.mu.Unlock()
	if !still {
		return false, nil
	}
	rec, err := s.machine(holder)
	if err != nil {
		return true, err
	}
	m := *rec.Machine
	switch m.State {
	case StateCreated, StateStopped, StatePaused:
		return true, s.putDiskSnapshot(ctx, disk, snapshot, request)
	case StateRunning:
		return true, s.snapshotRunningDisk(ctx, rec, disk, snapshot, request)
	}
	return true, fail(ErrPreconditionFailed, "disk %q is held by %s Machine %q; only a Created, Stopped, Paused or Running Machine's disk is snapshotted", disk, m.State, holder)
}

// snapshotRunningDisk copies the disk of a Running Machine with the guest paused,
// so the file does not change under the copy, and returns the Machine to
// Running. The copy is crash-consistent: the guest has not flushed what it holds
// in memory.
func (s *service) snapshotRunningDisk(ctx context.Context, rec *Record, disk DiskID, snapshot DiskSnapshotID, request RequestID) error {
	m := *rec.Machine
	if err := s.driver.Pause(ctx, m.ID); err != nil {
		return fail(ErrFailed, "pause for the disk snapshot: %v", err)
	}
	put := s.putDiskSnapshot(ctx, disk, snapshot, request)
	// Returning the guest to Running is compensation for the pause, whatever
	// became of the copy.
	ctx, cancel := s.compensating(ctx)
	defer cancel()
	if err := s.driver.Resume(ctx, m.ID); err != nil {
		reason := fmt.Sprintf("the Machine could not be returned to Running after the disk snapshot: %v", err)
		if put == nil {
			if rmErr := s.diskSnaps.Remove(ctx, snapshot); rmErr != nil {
				reason += fmt.Sprintf("; the disk snapshot could not be removed: %v", rmErr)
			}
		}
		faulted := m.clone()
		faulted.State = StateFaulted
		faulted.FailureReason = reason
		if err := s.commit(ctx, rec, faulted); err != nil {
			return s.unrecorded(rec, err)
		}
		return fail(ErrFailed, "%s", reason)
	}
	return put
}

// putDiskSnapshot has the store copy the disk's file, whose holder, if any, is
// quiet, and records what the disk is.
func (s *service) putDiskSnapshot(ctx context.Context, disk DiskID, snapshot DiskSnapshotID, request RequestID) error {
	e, err := s.disks.Get(ctx, disk)
	switch {
	case errors.Is(err, ErrDiskAbsent):
		return fail(ErrNotFound, "disk %q is not held by the node", disk)
	case err != nil:
		return fail(ErrFailed, "read disk %q: %v", disk, err)
	}
	path, err := s.locate(ctx, disk)
	if err != nil {
		return err
	}
	record := DiskSnapshot{ID: snapshot, Disk: disk, Request: request, SizeBytes: e.SizeBytes, Filesystem: e.Filesystem, CreatedAt: s.clock.Now()}
	switch err := s.diskSnaps.Put(ctx, record, path); {
	case errors.Is(err, ErrDiskSnapshotExists):
		return fail(ErrConflict, "disk snapshot %q exists under another request ID or another disk", snapshot)
	case err != nil:
		return fail(ErrFailed, "keep the disk snapshot: %v", err)
	}
	return nil
}

func (s *service) CreateDiskFromSnapshot(ctx context.Context, disk DiskID, request RequestID, sizeBytes uint64, filesystem Filesystem, snapshot DiskSnapshotID) error {
	switch {
	case !ValidDiskID(disk):
		return invalidDisk(disk)
	case !ValidDiskID(DiskID(snapshot)):
		return invalidDiskSnapshot(snapshot)
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	case sizeBytes == 0:
		return fail(ErrInvalid, "the disk size is zero")
	case filesystem != "" && filesystem != FilesystemNone && filesystem != FilesystemExt4:
		return fail(ErrInvalid, "filesystem %q is neither %q nor %q", filesystem, FilesystemNone, FilesystemExt4)
	}
	if err := s.requireDiskSnapshots(); err != nil {
		return err
	}
	defer s.diskLocks.lock(disk, true)()
	// Shared from the lookup until the copy is made, so a delete of the
	// snapshot and a SnapshotDisk still writing it both wait.
	defer s.diskSnapLocks.lock(snapshot, false)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	for attempt := 0; ; attempt++ {
		switch have, err := s.disks.Get(ctx, disk); {
		case err == nil:
			// A repeat is the disk the first call made, and the snapshot it was
			// made from need not be held any longer.
			if have.Request == request && have.FromSnapshot == snapshot && have.SizeBytes == sizeBytes && (filesystem == "" || filesystem == have.Filesystem) {
				return nil
			}
			return fail(ErrConflict, "disk %q exists under another request ID, snapshot, size or filesystem", disk)
		case !errors.Is(err, ErrDiskAbsent):
			return fail(ErrFailed, "look up disk %q: %v", disk, err)
		}
		snap, path, err := s.diskSnaps.Get(ctx, snapshot)
		switch {
		case errors.Is(err, ErrDiskSnapshotAbsent):
			return fail(ErrNotFound, "disk snapshot %q is not held by the node", snapshot)
		case err != nil:
			return fail(ErrFailed, "look up disk snapshot %q: %v", snapshot, err)
		case filesystem != "" && filesystem != snap.Filesystem:
			return fail(ErrInvalid, "disk snapshot %q holds filesystem %q and a disk made from it takes it, not %q", snapshot, snap.Filesystem, filesystem)
		case false:
			return fail(ErrInvalid, "disk snapshot %q is %d bytes and a disk made from it is no smaller: %d is smaller", snapshot, snap.SizeBytes, sizeBytes)
		}
		asked := DiskEntry{ID: disk, Request: request, SizeBytes: sizeBytes, Filesystem: snap.Filesystem, FromSnapshot: snapshot}
		err = s.disks.CreateFrom(ctx, asked, path)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrDiskInvalid):
			return fail(ErrInvalid, "%v", err)
		case errors.Is(err, ErrDiskExists) && attempt == 0:
			// A call made the disk between the lookup and the create: what it
			// made is what the lookup now answers for.
		default:
			return fail(ErrFailed, "create disk %q from disk snapshot %q: %v", disk, snapshot, err)
		}
	}
}

func (s *service) ListDiskSnapshots(ctx context.Context) ([]DiskSnapshotInfo, error) {
	if err := s.requireDiskSnapshots(); err != nil {
		return nil, err
	}
	all, err := s.diskSnaps.List(ctx)
	if err != nil {
		return nil, fail(ErrFailed, "list the disk snapshots: %v", err)
	}
	return all, nil
}

func (s *service) DeleteDiskSnapshot(ctx context.Context, snapshot DiskSnapshotID) error {
	if !ValidDiskID(DiskID(snapshot)) {
		return invalidDiskSnapshot(snapshot)
	}
	if err := s.requireDiskSnapshots(); err != nil {
		return err
	}
	// A SnapshotDisk call holds the lock exclusively and a Create disk call holds
	// it shared, so a delete that cannot take it now is beside a call using the
	// snapshot. It is refused and not made to wait: the snapshot may be gone by
	// then.
	unlock, ok := s.diskSnapLocks.tryLockExclusive(snapshot)
	if !ok {
		return fail(ErrPreconditionFailed, "disk snapshot %q is in use by a snapshot or create disk call", snapshot)
	}
	defer unlock()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	if err := s.diskSnaps.Remove(ctx, snapshot); err != nil {
		return fail(ErrFailed, "remove disk snapshot %q: %v", snapshot, err)
	}
	return nil
}
