package machine

import (
	"context"
	"errors"
	"fmt"
)

func (s *service) Snapshot(ctx context.Context, id MachineID, snapshot SnapshotID, request RequestID) error {
	defer s.lock(id)()
	switch {
	case id == "":
		return fail(ErrInvalid, "the Machine identifier is empty")
	case snapshot == "":
		return fail(ErrInvalid, "the snapshot identifier is empty")
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	}

	switch held, _, err := s.snapshots.Get(ctx, snapshot); {
	case err == nil:
		if held.Request == request && held.Machine == id {
			return nil
		}
		return fail(ErrConflict, "snapshot %q exists under another request ID or another Machine", snapshot)
	case !errors.Is(err, ErrSnapshotAbsent):
		return fail(ErrFailed, "look up snapshot %q: %v", snapshot, err)
	}

	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	m := *rec.Machine
	info := s.driver.Info()
	if !info.has(CapabilitySnapshot) {
		return fail(ErrUnsupported, "the Driver %s does not support snapshot", info.Name)
	}
	switch m.State {
	case StateRunning, StatePaused:
	default:
		return fail(ErrPreconditionFailed, "Machine %q is %s; snapshot needs Running or Paused", id, m.State)
	}

	// The guest is held paused for the whole capture, so its memory and its
	// disk are one pair. The Machine's recorded state does not change: it is
	// returned to what it was.
	resume := m.State == StateRunning
	if resume {
		if err := s.driver.Pause(ctx, id); err != nil {
			return fail(ErrFailed, "pause for the snapshot: %v", err)
		}
	}
	captured := s.capture(ctx, m, snapshot, request, info)
	if resume {
		if err := s.driver.Resume(ctx, id); err != nil {
			reason := fmt.Sprintf("the Machine could not be returned to Running after the snapshot: %v", err)
			if captured == nil {
				if rmErr := s.snapshots.Remove(ctx, snapshot); rmErr != nil {
					reason += fmt.Sprintf("; the snapshot could not be removed: %v", rmErr)
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
	}
	return captured
}

// capture has the Driver write the paused Machine and hands the files, with a
// copy of its root disk, to the Snapshot Store with the record of what they
// are compatible with. It returns nil only when the snapshot exists.
func (s *service) capture(ctx context.Context, m Machine, snapshot SnapshotID, request RequestID, info DriverInfo) error {
	taken, err := s.driver.Snapshot(ctx, m.ID)
	if err != nil {
		return fail(ErrFailed, "snapshot: %v", err)
	}
	disk, err := s.disks.Locate(ctx, m.RootDisk)
	if err != nil {
		return locateFailure(err, ErrDiskAbsent, "disk %q", m.RootDisk)
	}
	spec := Spec{Image: m.Image, Resources: m.Resources, RootDiskBytes: m.RootDiskBytes}
	record := Snapshot{
		ID: snapshot, Machine: m.ID, Request: request, Spec: spec, RootDiskUUID: m.RootDiskUUID,
		Compat: SnapshotCompat{Driver: info.Name, Version: info.Version, CPUModel: info.CPUModel, KernelDigest: taken.KernelDigest},
	}
	switch err := s.snapshots.Put(ctx, record, SnapshotFiles{State: taken.Files, RootDisk: disk}); {
	case errors.Is(err, ErrSnapshotExists):
		return fail(ErrConflict, "snapshot %q exists under another request ID or another Machine", snapshot)
	case err != nil:
		return fail(ErrFailed, "keep the snapshot: %v", err)
	}
	return nil
}

func (s *service) RestoreSnapshot(ctx context.Context, id MachineID, request RequestID, snapshot SnapshotID, policy NetworkPolicy) error {
	defer s.lock(id)()
	asked := Request{ID: request, Snapshot: snapshot, Spec: Spec{Policy: policy.clone()}}

	if rec, ok := s.lookup(id); ok {
		if rec.Request.ID == request && rec.Request.Snapshot == snapshot && rec.Request.Spec.equal(asked.Spec) {
			if rec.Request.Failure != nil {
				return rec.Request.Failure
			}
			return nil
		}
		if rec.Machine != nil {
			return fail(ErrConflict, "Machine %q exists under another request ID or other parameters", id)
		}
		// Only a failed attempt is recorded, and no Machine exists: the
		// identifier is free again.
	}
	switch {
	case id == "":
		return fail(ErrInvalid, "the Machine identifier is empty")
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	case snapshot == "":
		return fail(ErrInvalid, "the snapshot identifier is empty")
	}
	if err := policy.validate(); err != nil {
		return fail(ErrInvalid, "network policy: %v", err)
	}

	held, files, err := s.snapshots.Get(ctx, snapshot)
	switch {
	case errors.Is(err, ErrSnapshotAbsent):
		return fail(ErrNotFound, "snapshot %q is not held by the node", snapshot)
	case err != nil:
		return fail(ErrFailed, "look up snapshot %q: %v", snapshot, err)
	}
	info := s.driver.Info()
	if want := (SnapshotCompat{Driver: info.Name, Version: info.Version, CPUModel: info.CPUModel}); !held.Compat.compatible(want) {
		return fail(ErrPreconditionFailed, "snapshot %q was made by %s %s on %q; this node has %s %s on %q",
			snapshot, held.Compat.Driver, held.Compat.Version, held.Compat.CPUModel, want.Driver, want.Version, want.CPUModel)
	}
	if _, err := s.images.Locate(ctx, held.Spec.Image); err != nil {
		return locateFailure(err, ErrImageAbsent, "image %s", held.Spec.Image)
	}

	if err := s.permitter.PermitStart(ctx, id); err != nil {
		return fail(ErrPreconditionFailed, "restoring is not permitted: %v", err)
	}

	s.mu.Lock()
	uid, err := s.allocateUID(id)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	identity, err := s.network.Allocate(ctx, id)
	if err != nil {
		reason := s.undoRestore(ctx, id, "", false, fmt.Sprintf("allocate network: %v", err))
		return s.failRestore(ctx, id, asked, fail(ErrFailed, "%s", reason))
	}
	spec := held.Spec.clone()
	spec.Policy = policy.clone()
	root, err := s.disks.CreateRootFrom(ctx, DiskOwner{ID: id, UID: uid, GID: uid}, files.RootDisk)
	if err != nil {
		return s.failRestore(ctx, id, asked, fail(ErrFailed, "%s", s.undoRestore(ctx, id, "", false, fmt.Sprintf("create root disk: %v", err))))
	}
	s.claim(id, root) // a root disk is new, so no one else holds it
	m := Machine{ID: id, State: StateRunning, Spec: spec, RootDisk: root, RootDiskUUID: held.RootDiskUUID, User: User{UID: uid, GID: uid}, Network: identity}

	if failure, vmm := s.bringUp(ctx, m, files); failure != nil {
		reason := s.undoRestore(ctx, id, root, vmm, failure.Reason)
		if failure.Kind == ErrPreconditionFailed {
			// Nothing was begun that a repeat could not begin again, so the
			// refusal is not remembered, as Start's is not.
			return fail(ErrPreconditionFailed, "%s", reason)
		}
		return s.failRestore(ctx, id, asked, fail(failure.Kind, "%s", reason))
	}

	rec := &Record{ID: id, Request: Request{ID: request, Snapshot: snapshot, Spec: asked.Spec}}
	if err := s.store.Save(ctx, Record{ID: id, Machine: &m, Request: rec.Request}); err != nil {
		reason := s.undoRestore(ctx, id, root, true, fmt.Sprintf("record the Machine: %v", err))
		return s.failRestore(ctx, id, asked, fail(ErrFailed, "%s", reason))
	}
	s.mu.Lock()
	s.records[id] = rec
	s.mu.Unlock()
	s.setMachine(rec, &m)
	return nil
}

// bringUp applies the network, has the Driver describe the VMM process,
// starts it under its supervisor, and loads the snapshot into it. The
// supervisor start is the fencing service's, so it is the same step that
// hands over the current expiry. It returns the failure, and whether a VMM
// process exists when it returns.
func (s *service) bringUp(ctx context.Context, m Machine, files SnapshotFiles) (failure *Error, vmm bool) {
	id := m.ID
	boot := BootSpec{Resources: m.Resources}
	var err error
	if boot.RunDir, err = s.supervisor.RunDir(id); err != nil {
		return fail(ErrFailed, "name the run directory: %v", err), false
	}
	if boot.Devices, err = s.bootDevices(ctx, m); err != nil {
		var e *Error
		if errors.As(err, &e) {
			return e, false
		}
		return fail(ErrFailed, "%v", err), false
	}
	tap, err := s.network.Apply(ctx, id, m.Policy)
	if err != nil {
		return fail(ErrPreconditionFailed, "the network policy was not applied: %v", err), false
	}
	boot.Network = NetworkDevice{Tap: tap, MAC: m.Network.MAC}
	process, err := s.driver.DescribeRestore(ctx, id, boot, files.State)
	if err != nil {
		return fail(ErrFailed, "describe the VMM process: %v", err), false
	}
	if err := s.supervisor.Start(ctx, id, process.For(boot), m.User); err != nil {
		return fail(ErrFailed, "start the VMM process: %v", err), false
	}
	if err := s.driver.Restore(ctx, id); err != nil {
		return fail(ErrFailed, "load the snapshot: %v", err), true
	}
	return nil, true
}

// undoRestore ends what a failed restore made and returns the failure's
// reason, extended by anything that could not be undone. A VMM that cannot be
// ended is not undone around: its tap, disk and UID stay held, since taking
// them from a running VMM is worse than keeping them; no Machine exists then
// either, and the next start of the daemon ends a supervisor no record claims.
func (s *service) undoRestore(ctx context.Context, id MachineID, root DiskID, vmm bool, reason string) string {
	if vmm {
		if err := s.supervisor.Terminate(ctx, id); err != nil {
			return reason + fmt.Sprintf("; the VMM could not be ended, so nothing it holds was released: %v", err)
		}
	}
	if err := s.network.Release(ctx, id); err != nil {
		reason += fmt.Sprintf("; the network could not be released: %v", err)
	}
	if root != "" {
		if err := s.disks.Remove(ctx, root); err != nil {
			reason += fmt.Sprintf("; the root disk could not be removed: %v", err)
		}
		s.release(root)
	}
	s.mu.Lock()
	s.releaseUID(id)
	s.mu.Unlock()
	return reason
}

// failRestore records a failed restore under its request ID, so that a repeat
// returns the same outcome, and returns it. No Machine exists after it.
func (s *service) failRestore(ctx context.Context, id MachineID, asked Request, failure *Error) error {
	asked.Failure = failure
	rec := &Record{ID: id, Request: asked}
	s.mu.Lock()
	s.records[id] = rec
	s.mu.Unlock()
	if err := s.store.Save(ctx, *rec); err != nil {
		return fail(ErrFailed, "%s; the failure could not be recorded: %v", failure.Reason, err)
	}
	return failure
}

// compatible reports whether a snapshot made on c can be restored on a node
// that is on. The kernel digest is recorded and not compared.
func (c SnapshotCompat) compatible(on SnapshotCompat) bool {
	return c.Driver == on.Driver && c.Version == on.Version && c.CPUModel == on.CPUModel
}
