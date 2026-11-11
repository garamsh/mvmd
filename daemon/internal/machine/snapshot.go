package machine

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
)

// catchUpEntropy is the bytes of entropy a restore sends its guest, within the
// 32 to 256 ADR 0016 allows.
const catchUpEntropy = 64

func (s *service) Snapshot(ctx context.Context, id MachineID, snapshot SnapshotID, request RequestID) error {
	defer s.lock(id)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	switch {
	case id == "":
		return fail(ErrInvalid, "the Machine identifier is empty")
	case snapshot == "":
		return fail(ErrInvalid, "the snapshot identifier is empty")
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	}
	// No restore reads the snapshot until this call has finished with it: not
	// while the guest is still paused, and not if the call then fails.
	defer s.snapLocks.lock(snapshot, true)()

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
	if m.KernelDigest == "" {
		return fail(ErrPreconditionFailed, "Machine %q records no kernel digest, so a snapshot of it could not be restored; restart it, which records one", id)
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
		// Returning the guest to Running is compensation for the pause, whatever
		// became of the capture.
		ctx, cancel := s.compensating(ctx)
		defer cancel()
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
	disks, err := s.describeDisks(ctx, m.Disks)
	if err != nil {
		return err
	}
	spec := SnapshotSpec{Image: m.Image, Resources: m.Resources, Ceiling: m.Ceiling, BootMemoryBytes: m.BootMemoryBytes, RootDiskBytes: m.RootDiskBytes}
	record := Snapshot{
		ID: snapshot, Machine: m.ID, Request: request, Spec: spec, RootDiskUUID: m.RootDiskUUID, MAC: m.Network.MAC,
		Compat: SnapshotCompat{Driver: info.Name, Version: info.Version, CPUModel: info.CPUModel, KernelDigest: m.KernelDigest, KernelName: m.Kernel},
		Disks:  disks,
	}
	switch err := s.snapshots.Put(ctx, record, SnapshotFiles{State: taken.Files, RootDisk: disk}); {
	case errors.Is(err, ErrSnapshotExists):
		return fail(ErrConflict, "snapshot %q exists under another request ID or another Machine", snapshot)
	case err != nil:
		return fail(ErrFailed, "keep the snapshot: %v", err)
	}
	return nil
}

// describeDisks records what each attached disk's file shows while the guest is
// paused, the point the snapshot and its disks are one pair (ADR 0041).
func (s *service) describeDisks(ctx context.Context, attached []DiskAttachment) ([]SnapshotDisk, error) {
	var disks []SnapshotDisk
	for _, a := range attached {
		file, err := s.disks.Stat(ctx, a.Disk)
		if err != nil {
			return nil, locateFailure(err, ErrDiskAbsent, "disk %q", a.Disk)
		}
		disks = append(disks, SnapshotDisk{Disk: a.Disk, ReadOnly: a.ReadOnly, SizeBytes: file.SizeBytes, ModTime: file.ModTime})
	}
	return disks, nil
}

func (s *service) RestoreSnapshot(ctx context.Context, id MachineID, request RequestID, snapshot SnapshotID, policy NetworkPolicy) error {
	defer s.lock(id)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
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
	case CheckMachineID(id) != nil:
		return fail(ErrInvalid, "the Machine identifier: %v", CheckMachineID(id))
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	case snapshot == "":
		return fail(ErrInvalid, "the snapshot identifier is empty")
	}
	// Shared from the lookup until the files are no longer read, so a Snapshot
	// of it that is still running or that fails is never restored from, and
	// restores of one snapshot still run together.
	defer s.snapLocks.lock(snapshot, false)()
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
	if err := s.overLimits(held.Spec.Resources, held.Spec.Ceiling, ErrPreconditionFailed); err != nil {
		return err
	}
	kernel, ok := s.kernels.matching(held.Compat.KernelDigest, held.Compat.KernelName)
	if !ok {
		return fail(ErrPreconditionFailed, "snapshot %q was made with a kernel of digest %q, which the node does not offer", snapshot, held.Compat.KernelDigest)
	}
	// Shared until the record is set, as Create holds it.
	defer s.imageLocks.lock(held.Spec.Image, false)()
	if _, err := s.images.Locate(ctx, held.Spec.Image); err != nil {
		return locateFailure(err, ErrImageAbsent, "image %s", held.Spec.Image)
	}

	if err := s.permitter.PermitStart(ctx, id); err != nil {
		return fail(ErrPreconditionFailed, "restoring is not permitted: %v", err)
	}

	// The disks are taken before anything is created, so a refusal leaves
	// nothing behind (ADR 0041).
	disks, err := s.takeDisks(ctx, id, held.Disks)
	if err != nil {
		return err
	}
	uid, err := s.uids.AllocateMachine(id)
	if err != nil {
		s.release(disks...)
		return err
	}
	identity, err := s.network.Allocate(ctx, id, held.MAC)
	if err != nil {
		reason := s.undoRestore(ctx, id, "", disks, fmt.Sprintf("allocate network: %v", err))
		return s.failRestore(ctx, id, asked, fail(ErrFailed, "%s", reason))
	}
	spec := Spec{Image: held.Spec.Image, Resources: held.Spec.Resources, Ceiling: held.Spec.Ceiling, RootDiskBytes: held.Spec.RootDiskBytes, Kernel: kernel, Policy: policy.clone()}
	// The root disk's name is held before its file is published, so Delete
	// disk cannot remove it between the two.
	if taken, holder, ok := s.claim(id, RootDiskID(id)); !ok {
		reason := s.undoRestore(ctx, id, "", disks, fmt.Sprintf("disk %q is held by Machine %q", taken, holder))
		return fail(ErrPreconditionFailed, "%s", reason)
	}
	root, err := s.disks.CreateRootFrom(ctx, DiskOwner{ID: id, UID: uid, GID: uid}, files.RootDisk)
	if err != nil {
		reason := s.undoRestore(ctx, id, "", disks, fmt.Sprintf("create root disk: %v", err))
		s.release(RootDiskID(id))
		return s.failRestore(ctx, id, asked, fail(ErrFailed, "%s", reason))
	}
	// The disks are the Machine's before any Driver opens them (ADR 0038).
	if err := s.give(ctx, DiskOwner{ID: id, UID: uid, GID: uid}, disks...); err != nil {
		reason := s.undoRestore(ctx, id, root, disks, err.Error())
		return s.failRestore(ctx, id, asked, fail(ErrFailed, "%s", reason))
	}
	spec.Disks = attachments(held.Disks)
	m := Machine{ID: id, State: StateRunning, Spec: spec, BootMemoryBytes: held.Spec.BootMemoryBytes, RootDisk: root, RootDiskUUID: held.RootDiskUUID, KernelDigest: held.Compat.KernelDigest, User: User{UID: uid, GID: uid}, Network: identity}

	res := s.bringUp(ctx, s.restorePlan(m, request, files))
	if res.failure != nil {
		return s.restoreFailed(ctx, m, root, asked, res)
	}

	rec := &Record{ID: id, Request: Request{ID: request, Snapshot: snapshot, Spec: asked.Spec}}
	if err := s.store.Save(ctx, Record{ID: id, Machine: &m, Request: rec.Request}); err != nil {
		return s.restoreFailed(ctx, m, root, asked, s.unwind(ctx, id, res.begun, fail(ErrFailed, "record the Machine: %v", err)))
	}
	s.mu.Lock()
	s.records[id] = rec
	s.mu.Unlock()
	s.setMachine(rec, &m)
	return nil
}

// takeDisks takes the holder slot of each disk a snapshot recorded and checks
// that its file shows what the record does, or refuses naming the disk and
// whether it is absent, held or changed. A refusal holds no slot.
func (s *service) takeDisks(ctx context.Context, id MachineID, recorded []SnapshotDisk) ([]DiskID, error) {
	disks := make([]DiskID, 0, len(recorded))
	for _, d := range recorded {
		disks = append(disks, d.Disk)
	}
	if taken, holder, ok := s.claim(id, disks...); !ok {
		if holder == deleting {
			return nil, fail(ErrPreconditionFailed, "disk %q is being deleted", taken)
		}
		return nil, fail(ErrPreconditionFailed, "disk %q is held by Machine %q", taken, holder)
	}
	for _, d := range recorded {
		file, err := s.disks.Stat(ctx, d.Disk)
		var refusal error
		switch {
		case errors.Is(err, ErrDiskAbsent):
			refusal = fail(ErrPreconditionFailed, "disk %q is absent: the node does not hold it", d.Disk)
		case err != nil:
			refusal = fail(ErrFailed, "read disk %q: %v", d.Disk, err)
		case file.SizeBytes != d.SizeBytes || !file.ModTime.Equal(d.ModTime):
			refusal = fail(ErrPreconditionFailed, "disk %q is changed: it was written after the snapshot", d.Disk)
		}
		if refusal != nil {
			s.release(disks...)
			return nil, refusal
		}
	}
	return disks, nil
}

// attachments lists the recorded disks as attached.
func attachments(recorded []SnapshotDisk) []DiskAttachment {
	var out []DiskAttachment
	for _, d := range recorded {
		out = append(out, DiskAttachment{Disk: d.Disk, ReadOnly: d.ReadOnly})
	}
	return out
}

func diskIDs(attached []DiskAttachment) []DiskID {
	var out []DiskID
	for _, a := range attached {
		out = append(out, a.Disk)
	}
	return out
}

// restorePlan is what a restore chooses of the boot pipeline: the snapshot's
// description, its load into the VMM, and the guest's catch-up. The guest still
// holds the snapshot's memory, clock and addresses, and the Machine is not
// Running, nor reported so, until it has caught up (ADR 0016).
func (s *service) restorePlan(m Machine, request RequestID, files SnapshotFiles) bootPlan {
	id := m.ID
	return bootPlan{
		machine:       m,
		restoredDisks: m.Disks,
		describe: func(ctx context.Context, boot BootSpec) (ProcessDescription, error) {
			return s.driver.DescribeRestore(ctx, id, boot, files.State)
		},
		vmm: func(ctx context.Context, boot BootSpec) error {
			if err := s.driver.Restore(ctx, id, boot); err != nil {
				return fmt.Errorf("load the snapshot: %w", err)
			}
			return nil
		},
		guest: func(ctx context.Context, _ LocatedImage) error {
			// Entropy is drawn afresh here: two restores sent the same bytes
			// would share random state again.
			entropy := make([]byte, catchUpEntropy)
			if _, err := rand.Read(entropy); err != nil {
				return fmt.Errorf("draw entropy for the guest: %w", err)
			}
			catchUp := CatchUp{RestoreID: request, Entropy: entropy, HostTime: s.clock.Now(), Network: m.Network.guestNetwork()}
			if err := s.guest.CatchUp(ctx, id, catchUp); err != nil {
				return fmt.Errorf("catch the guest up: %w", err)
			}
			return nil
		},
		guestWhile: "catching up",
	}
}

// restoreFailed ends a failed restore, whose VMM the pipeline has already
// ended: the Machine does not exist, or is Faulted if its VMM could not be
// ended.
func (s *service) restoreFailed(ctx context.Context, m Machine, root DiskID, asked Request, res bootResult) error {
	reason := res.reason()
	if res.endErr != nil {
		return s.faultedRestore(ctx, m, asked, reason+"; nothing it holds was released")
	}
	reason = s.undoRestore(ctx, m.ID, root, diskIDs(m.Disks), reason)
	if res.failure.Kind == ErrPreconditionFailed {
		// Nothing was begun that a repeat could not begin again, so the
		// refusal is not remembered, as Start's is not.
		return fail(ErrPreconditionFailed, "%s", reason)
	}
	return s.failRestore(ctx, m.ID, asked, fail(res.failure.Kind, "%s", reason))
}

// undoRestore releases what a failed restore allocated and returns the
// failure's reason, extended by anything that could not be undone. It is not
// called for a VMM that could not be ended: its tap, disk and UID stay held,
// since taking them from a running VMM is worse than keeping them, and the
// caller makes the Machine Faulted, which Delete releases.
func (s *service) undoRestore(ctx context.Context, id MachineID, root DiskID, disks []DiskID, reason string) string {
	ctx, cancel := s.compensating(ctx)
	defer cancel()
	if err := s.reclaim(ctx, disks...); err != nil {
		reason += fmt.Sprintf("; %v", err)
	}
	s.release(disks...)
	if err := s.network.Release(ctx, id); err != nil {
		reason += fmt.Sprintf("; the network could not be released: %v", err)
	}
	if root != "" {
		if err := s.disks.Remove(ctx, root); err != nil {
			reason += fmt.Sprintf("; the root disk could not be removed: %v", err)
		}
		s.release(root)
	}
	s.uids.ReleaseMachine(id)
	return reason
}

// faultedRestore is the outcome of a failed restore whose VMM could not be
// ended: the Machine exists, Faulted, holding what the restore allocated, and
// Delete releases it as it does for any Faulted Machine. The failure is
// remembered under the request ID, so a repeat returns it.
func (s *service) faultedRestore(ctx context.Context, m Machine, asked Request, reason string) error {
	ctx, cancel := s.compensating(ctx)
	defer cancel()
	m.State = StateFaulted
	m.FailureReason = reason
	asked.Failure = fail(ErrFailed, "%s", reason)
	if err := s.store.Save(ctx, Record{ID: m.ID, Machine: &m, Request: asked}); err != nil {
		reason += fmt.Sprintf("; the Machine could not be recorded: %v", err)
		asked.Failure = fail(ErrFailed, "%s", reason)
		m.FailureReason = reason
	}
	rec := &Record{ID: m.ID, Request: asked}
	s.mu.Lock()
	s.records[m.ID] = rec
	s.mu.Unlock()
	s.setMachine(rec, &m)
	return asked.Failure
}

// failRestore records a failed restore under its request ID, so that a repeat
// returns the same outcome, and returns it. No Machine exists after it.
func (s *service) failRestore(ctx context.Context, id MachineID, asked Request, failure *Error) error {
	ctx, cancel := s.compensating(ctx)
	defer cancel()
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

func (s *service) ListSnapshots(ctx context.Context) ([]Snapshot, error) {
	all, err := s.snapshots.List(ctx)
	if err != nil {
		return nil, fail(ErrFailed, "list the snapshots: %v", err)
	}
	return all, nil
}

func (s *service) DeleteSnapshot(ctx context.Context, snapshot SnapshotID) error {
	if snapshot == "" {
		return fail(ErrInvalid, "the snapshot identifier is empty")
	}
	// A Snapshot call holds the lock exclusively and a restore holds it shared,
	// so a delete that cannot take it now is beside a call using the snapshot.
	// It is refused and not made to wait: the snapshot may be gone by then.
	unlock, ok := s.snapLocks.tryLockExclusive(snapshot)
	if !ok {
		return fail(ErrPreconditionFailed, "snapshot %q is in use by a snapshot or restore call", snapshot)
	}
	defer unlock()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	if err := s.snapshots.Remove(ctx, snapshot); err != nil {
		return fail(ErrFailed, "remove snapshot %q: %v", snapshot, err)
	}
	return nil
}

// compatible reports whether a snapshot made on c can be restored on a node
// that is on. The kernel digest is recorded and not compared.
func (c SnapshotCompat) compatible(on SnapshotCompat) bool {
	return c.Driver == on.Driver && c.Version == on.Version && c.CPUModel == on.CPUModel
}
