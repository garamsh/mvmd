package machine

import (
	"context"
	"errors"
	"fmt"
	"time"
)

func (s *service) Create(ctx context.Context, id MachineID, request RequestID, spec Spec) error {
	defer s.lock(id)()
	spec = spec.clone()

	if rec, ok := s.lookup(id); ok {
		if rec.Request.ID == request && rec.Request.Spec.equal(spec) {
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
	if err := validateCreate(id, request, spec); err != nil {
		return err
	}

	if _, err := s.images.Locate(ctx, spec.Image); err != nil {
		if errors.Is(err, ErrImageAbsent) {
			return fail(ErrNotFound, "image %s is not on the node", spec.Image)
		}
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "locate image: %v", err))
	}
	var disks []DiskID
	for _, a := range spec.Disks {
		if _, err := s.disks.Locate(ctx, a.Disk); err != nil {
			if errors.Is(err, ErrDiskAbsent) {
				return fail(ErrNotFound, "disk %q is not held by the node", a.Disk)
			}
			return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "locate disk %q: %v", a.Disk, err))
		}
		disks = append(disks, a.Disk)
	}
	if taken, holder, ok := s.claim(id, disks...); !ok {
		return fail(ErrPreconditionFailed, "disk %q is held by Machine %q", taken, holder)
	}

	s.mu.Lock()
	uid, err := s.allocateUID(id)
	if err != nil {
		s.mu.Unlock()
		s.release(disks...)
		return err
	}
	s.mu.Unlock()

	identity, err := s.network.Allocate(ctx, id)
	if err != nil {
		s.release(disks...)
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "allocate network: %v", err))
	}
	root, uuid, err := s.disks.CreateRoot(ctx, DiskOwner{ID: id, UID: uid, GID: uid}, spec.RootDiskBytes)
	if err != nil {
		reason := fmt.Sprintf("create root disk: %v", err)
		s.release(append(disks, root)...)
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "%s", s.undoCreate(ctx, id, "", reason)))
	}
	s.claim(id, root) // a root disk is new, so no one else holds it
	m := Machine{ID: id, State: StateCreated, Spec: spec, RootDisk: root, RootDiskUUID: uuid, User: User{UID: uid, GID: uid}, Network: identity}
	rec := &Record{ID: id, Machine: &m, Request: Request{ID: request, Spec: spec}}
	if err := s.store.Save(ctx, *rec); err != nil {
		reason := fmt.Sprintf("record the Machine: %v", err)
		s.release(append(disks, root)...)
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "%s", s.undoCreate(ctx, id, root, reason)))
	}
	s.mu.Lock()
	s.records[id] = rec
	s.mu.Unlock()
	return nil
}

func validateCreate(id MachineID, request RequestID, spec Spec) error {
	switch {
	case id == "":
		return fail(ErrInvalid, "the Machine identifier is empty")
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	case spec.Image == "":
		return fail(ErrInvalid, "the image digest is empty")
	case spec.Resources.VCPUs == 0 || spec.Resources.MemoryBytes == 0:
		return fail(ErrInvalid, "the Machine needs at least one vCPU and some memory")
	case spec.RootDiskBytes == 0:
		return fail(ErrInvalid, "the root disk size is zero")
	}
	seen := make(map[DiskID]bool)
	for _, a := range spec.Disks {
		if seen[a.Disk] {
			return fail(ErrInvalid, "disk %q is listed twice", a.Disk)
		}
		seen[a.Disk] = true
	}
	if err := spec.Policy.validate(); err != nil {
		return fail(ErrInvalid, "network policy: %v", err)
	}
	return nil
}

// undoCreate releases what a failed Create made and returns the failure's
// reason, extended by anything that could not be undone. It also releases the
// UID allocated for id.
func (s *service) undoCreate(ctx context.Context, id MachineID, root DiskID, reason string) string {
	if root != "" {
		if err := s.disks.Remove(ctx, root); err != nil {
			reason += fmt.Sprintf("; the root disk could not be removed: %v", err)
		}
	}
	if err := s.network.Release(ctx, id); err != nil {
		reason += fmt.Sprintf("; the network could not be released: %v", err)
	}
	s.mu.Lock()
	s.releaseUID(id)
	s.mu.Unlock()
	return reason
}

// failCreate records a failed Create under its request ID, so that a repeat
// returns the same outcome, and returns it. No Machine exists after it.
func (s *service) failCreate(ctx context.Context, id MachineID, request RequestID, spec Spec, failure *Error) error {
	rec := &Record{ID: id, Request: Request{ID: request, Spec: spec, Failure: failure}}
	s.mu.Lock()
	s.records[id] = rec
	s.mu.Unlock()
	if err := s.store.Save(ctx, *rec); err != nil {
		return fail(ErrFailed, "%s; the failure could not be recorded: %v", failure.Reason, err)
	}
	return failure
}

func (s *service) Start(ctx context.Context, id MachineID) error {
	defer s.lock(id)()
	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	m := *rec.Machine
	switch m.State {
	case StateRunning:
		return nil
	case StateCreated, StateStopped:
	default:
		return fail(ErrPreconditionFailed, "Machine %q is %s; start needs Created or Stopped", id, m.State)
	}

	if err := s.permitter.PermitStart(ctx, id); err != nil {
		return fail(ErrPreconditionFailed, "starting is not permitted: %v", err)
	}
	boot := BootSpec{Resources: m.Resources}
	if boot.Devices, err = s.bootDevices(ctx, m); err != nil {
		return err
	}
	tap, err := s.network.Apply(ctx, id, m.Policy)
	if err != nil {
		return fail(ErrPreconditionFailed, "the network policy was not applied: %v", err)
	}
	boot.Network = NetworkDevice{Tap: tap, MAC: m.Network.MAC}
	process, err := s.driver.Describe(ctx, id, boot)
	if err != nil {
		return s.withdrawn(ctx, id, fail(ErrFailed, "describe the VMM process: %v", err))
	}
	if err := s.supervisor.Start(ctx, id, process.For(boot), m.User); err != nil {
		return s.withdrawn(ctx, id, fail(ErrFailed, "start the VMM process: %v", err))
	}
	if err := s.driver.Boot(ctx, id); err != nil {
		return s.bootFailed(ctx, rec, err)
	}

	next := m.clone()
	next.State = StateRunning
	next.FailureReason = ""
	if err := s.commit(ctx, rec, next); err != nil {
		return s.unrecorded(rec, err)
	}
	return nil
}

// withdrawn removes the network a start applied before it failed, with no VMM
// process to hold it, and returns the failure, extended by a removal that
// failed.
func (s *service) withdrawn(ctx context.Context, id MachineID, failure *Error) error {
	if err := s.network.Withdraw(ctx, id); err != nil {
		return fail(failure.Kind, "%s; the network could not be removed: %v", failure.Reason, err)
	}
	return failure
}

// bootDevices lists a Machine's block devices in boot order: its image
// read-only, its root disk, then its attached disks.
func (s *service) bootDevices(ctx context.Context, m Machine) ([]BlockDevice, error) {
	path, err := s.images.Locate(ctx, m.Image)
	if err != nil {
		return nil, locateFailure(err, ErrImageAbsent, "image %s", m.Image)
	}
	devices := []BlockDevice{{Path: path, ReadOnly: true}}
	attached := append([]DiskAttachment{{Disk: m.RootDisk}}, m.Disks...)
	for _, a := range attached {
		path, err := s.disks.Locate(ctx, a.Disk)
		if err != nil {
			return nil, locateFailure(err, ErrDiskAbsent, "disk %q", a.Disk)
		}
		devices = append(devices, BlockDevice{Disk: a.Disk, Path: path, ReadOnly: a.ReadOnly})
	}
	return devices, nil
}

func locateFailure(err, absent error, what string, args ...any) error {
	what = fmt.Sprintf(what, args...)
	if errors.Is(err, absent) {
		return fail(ErrPreconditionFailed, "%s is not on the node", what)
	}
	return fail(ErrFailed, "locate %s: %v", what, err)
}

// bootFailed ends a boot that began and failed: the VMM is ended and the
// Machine is Stopped with the reason, or Faulted if the VMM cannot be ended.
func (s *service) bootFailed(ctx context.Context, rec *Record, boot error) error {
	next := rec.Machine.clone()
	next.State = StateStopped
	next.FailureReason = fmt.Sprintf("boot failed: %v", boot)
	if err := s.supervisor.Terminate(ctx, rec.ID); err != nil {
		next.State = StateFaulted
		next.FailureReason += fmt.Sprintf("; the VMM could not be ended: %v", err)
	} else if err := s.network.Withdraw(ctx, rec.ID); err != nil {
		next.State = StateFaulted
		next.FailureReason += fmt.Sprintf("; the network could not be removed: %v", err)
	}
	if err := s.commit(ctx, rec, next); err != nil {
		return s.unrecorded(rec, err)
	}
	return fail(ErrFailed, "%s", next.FailureReason)
}

func (s *service) Stop(ctx context.Context, id MachineID, grace time.Duration) error {
	return s.stop(ctx, id, grace, true)
}

func (s *service) ForceStop(ctx context.Context, id MachineID) error {
	return s.stop(ctx, id, 0, false)
}

// stop ends a Machine's VMM, asking the guest first only when graceful.
func (s *service) stop(ctx context.Context, id MachineID, grace time.Duration, graceful bool) error {
	defer s.lock(id)()
	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	m := *rec.Machine
	if m.State == StateCreated || m.State == StateStopped {
		return nil
	}

	// A guest cannot be trusted to stop, so its answer to the request does not
	// decide the outcome: the VMM is ended either way.
	if graceful {
		_ = s.driver.Shutdown(ctx, id, grace)
	}
	if err := s.supervisor.Terminate(ctx, id); err != nil {
		if errors.Is(err, ErrNotAttempted) {
			return fail(ErrFailed, "the stop never began: %v", err)
		}
		next := m.clone()
		next.State = StateFaulted
		next.FailureReason = fmt.Sprintf("the stop began and could not be confirmed: %v", err)
		if err := s.commit(ctx, rec, next); err != nil {
			return s.unrecorded(rec, err)
		}
		return fail(ErrFailed, "%s", next.FailureReason)
	}

	// The VMM has ended, so its tap goes (ADR 0011).
	next := m.clone()
	next.State = StateStopped
	if err := s.network.Withdraw(ctx, id); err != nil {
		next.State = StateFaulted
		next.FailureReason = fmt.Sprintf("the VMM ended and the network could not be removed: %v", err)
	}
	if err := s.commit(ctx, rec, next); err != nil {
		return s.unrecorded(rec, err)
	}
	if next.State == StateFaulted {
		return fail(ErrFailed, "%s", next.FailureReason)
	}
	return nil
}

func (s *service) Pause(ctx context.Context, id MachineID) error {
	return s.move(ctx, id, "pause", StateRunning, StatePaused, s.driver.Pause)
}

func (s *service) Resume(ctx context.Context, id MachineID) error {
	return s.move(ctx, id, "resume", StatePaused, StateRunning, s.driver.Resume)
}

// move takes a Machine from one state to the other through the Driver.
// Already being in the target state succeeds; a failure leaves the state it
// was in.
func (s *service) move(ctx context.Context, id MachineID, call string, from, to State, effect func(context.Context, MachineID) error) error {
	defer s.lock(id)()
	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	switch rec.Machine.State {
	case to:
		return nil
	case from:
	default:
		return fail(ErrPreconditionFailed, "Machine %q is %s; %s needs %s", id, rec.Machine.State, call, from)
	}
	if err := effect(ctx, id); err != nil {
		return fail(ErrFailed, "%s: %v", call, err)
	}
	next := rec.Machine.clone()
	next.State = to
	if err := s.commit(ctx, rec, next); err != nil {
		return s.unrecorded(rec, err)
	}
	return nil
}

func (s *service) Delete(ctx context.Context, id MachineID) error {
	defer s.lock(id)()
	rec, err := s.machine(id)
	if err != nil {
		return nil
	}
	switch rec.Machine.State {
	case StateRunning, StatePaused:
		return fail(ErrPreconditionFailed, "Machine %q is %s; delete needs Created, Stopped or Faulted", id, rec.Machine.State)
	case StateFaulted:
		// The disks are released only once the VMM has ended.
		if err := s.supervisor.Terminate(ctx, id); err != nil {
			return fail(ErrFailed, "the VMM of Faulted Machine %q could not be ended: %v", id, err)
		}
	}
	if err := s.network.Release(ctx, id); err != nil {
		return fail(ErrFailed, "release the network: %v", err)
	}
	// A Machine restored from an unreadable record has no root disk name:
	// the disk file is left for the operator, since naming one to remove
	// would be a guess.
	if rec.Machine.RootDisk != "" {
		if err := s.disks.Remove(ctx, rec.Machine.RootDisk); err != nil {
			return fail(ErrFailed, "remove the root disk: %v", err)
		}
	}
	if err := s.store.Remove(ctx, id); err != nil {
		return fail(ErrFailed, "remove the record: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.records, id)
	delete(s.held, rec.Machine.RootDisk)
	for _, a := range rec.Machine.Disks {
		delete(s.held, a.Disk)
	}
	s.releaseUID(id)
	return nil
}
