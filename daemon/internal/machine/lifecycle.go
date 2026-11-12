package machine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

func (s *service) Create(ctx context.Context, id MachineID, request RequestID, spec Spec) error {
	defer s.lock(id)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
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
	name, _, ok := s.kernels.resolve(spec.Kernel)
	if !ok {
		if spec.Kernel == "" {
			return fail(ErrPreconditionFailed, "the node offers no default kernel")
		}
		return fail(ErrInvalid, "kernel %q is not offered by the node", spec.Kernel)
	}
	held := spec
	held.Kernel = name
	held.Ceiling = spec.Ceiling.OrElse(spec.Resources)
	if err := s.checkCeiling(held.Resources, held.Ceiling, spec.Ceiling != Resources{}); err != nil {
		return err
	}

	if len(spec.Disks) > 0 && !s.driver.Info().has(CapabilityDiskHotplug) {
		return fail(ErrUnsupported, "the Driver cannot hot-plug, so it cannot start a Machine with a disk")
	}

	// Shared from Locate until the record is set, so RemoveImage cannot pass
	// between the two.
	defer s.imageLocks.lock(spec.Image, false)()
	located, err := s.images.Locate(ctx, spec.Image)
	if err != nil {
		if errors.Is(err, ErrImageAbsent) {
			return fail(ErrNotFound, "image %s is not on the node", spec.Image)
		}
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "locate image: %v", err))
	}
	if _, _, err := spec.runOn(spec.Image, located.Run); err != nil {
		return err
	}
	var disks []DiskID
	for _, a := range spec.Disks {
		if _, err := s.disks.Locate(ctx, a.Disk); err != nil {
			if errors.Is(err, ErrDiskAbsent) {
				return fail(ErrNotFound, "disk %q is not held by the node", a.Disk)
			}
			return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "locate disk %q: %v", a.Disk, err))
		}
		if err := s.checkMountable(ctx, a); err != nil {
			// An adapter failure is recorded under the request as locate is, so a
			// repeat answers the same; invalid and not found are not.
			if failure := (*Error)(nil); errors.As(err, &failure) && errors.Is(failure, ErrFailed) {
				return failure
			}
			return err
		}
		disks = append(disks, a.Disk)
	}
	// The root disk's name is held with the attached disks, before its file is
	// published, so Delete disk cannot remove it between the two.
	disks = append(disks, RootDiskID(id))
	if taken, holder, ok := s.claim(id, disks...); !ok {
		return fail(ErrPreconditionFailed, "disk %q is held by Machine %q", taken, holder)
	}

	uid, err := s.uids.AllocateMachine(id)
	if err != nil {
		s.release(disks...)
		return err
	}

	identity, err := s.network.Allocate(ctx, id, "")
	if errors.Is(err, ErrNoFreeAddress) {
		s.release(disks...)
		s.uids.ReleaseMachine(id)
		return fail(ErrPreconditionFailed, "allocate network: %v", err)
	}
	if err != nil {
		s.release(disks...)
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "allocate network: %v", err))
	}
	owner := DiskOwner{ID: id, UID: uid, GID: uid}
	root, uuid, err := s.disks.CreateRoot(ctx, owner, spec.RootDiskBytes)
	if err != nil {
		reason := fmt.Sprintf("create root disk: %v", err)
		s.release(disks...)
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "%s", s.undoCreate(ctx, id, "", nil, reason)))
	}
	// The attached disks are the Machine's before any Driver opens them
	// (ADR 0038); the root disk was made its own.
	attached := disks[:len(disks)-1]
	if err := s.give(ctx, owner, attached...); err != nil {
		s.release(disks...)
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "%s", s.undoCreate(ctx, id, root, nil, err.Error())))
	}
	m := Machine{ID: id, State: StateCreated, Spec: held, RootDisk: root, RootDiskUUID: uuid, User: User{UID: uid, GID: uid}, Network: identity}
	rec := &Record{ID: id, Request: Request{ID: request, Spec: spec}}
	if err := s.store.Save(ctx, Record{ID: id, Machine: &m, Request: rec.Request}); err != nil {
		reason := fmt.Sprintf("record the Machine: %v", err)
		s.release(disks...)
		return s.failCreate(ctx, id, request, spec, fail(ErrFailed, "%s", s.undoCreate(ctx, id, root, attached, reason)))
	}
	s.mu.Lock()
	s.records[id] = rec
	s.mu.Unlock()
	s.setMachine(rec, &m)
	return nil
}

func validateCreate(id MachineID, request RequestID, spec Spec) error {
	idErr := CheckMachineID(id)
	switch {
	case id == "":
		return fail(ErrInvalid, "the Machine identifier is empty")
	case idErr != nil:
		return fail(ErrInvalid, "the Machine identifier: %v", idErr)
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
	mounts := make(map[string]bool)
	for _, a := range spec.Disks {
		if !ValidDiskID(a.Disk) {
			return invalidDisk(a.Disk)
		}
		if seen[a.Disk] {
			return fail(ErrInvalid, "disk %q is listed twice", a.Disk)
		}
		seen[a.Disk] = true
		if err := checkMountPath(a.MountPath); err != nil {
			return err
		}
		if a.MountPath != "" {
			if mounts[a.MountPath] {
				return fail(ErrInvalid, "mount path %q is given twice", a.MountPath)
			}
			mounts[a.MountPath] = true
		}
	}
	if err := spec.Policy.validate(); err != nil {
		return fail(ErrInvalid, "network policy: %v", err)
	}
	return spec.validateRun()
}

// undoCreate releases what a failed Create made and returns the failure's
// reason, extended by anything that could not be undone. It also releases the
// UID allocated for id and gives the attached disks back to mvmd.
func (s *service) undoCreate(ctx context.Context, id MachineID, root DiskID, attached []DiskID, reason string) string {
	ctx, cancel := s.compensating(ctx)
	defer cancel()
	if err := s.reclaim(ctx, attached...); err != nil {
		reason += fmt.Sprintf("; %v", err)
	}
	if root != "" {
		if err := s.disks.Remove(ctx, root); err != nil {
			reason += fmt.Sprintf("; the root disk could not be removed: %v", err)
		}
	}
	if err := s.network.Release(ctx, id); err != nil {
		reason += fmt.Sprintf("; the network could not be released: %v", err)
	}
	s.uids.ReleaseMachine(id)
	return reason
}

// failCreate records a failed Create under its request ID, so that a repeat
// returns the same outcome, and returns it. No Machine exists after it.
func (s *service) failCreate(ctx context.Context, id MachineID, request RequestID, spec Spec, failure *Error) error {
	ctx, cancel := s.compensating(ctx)
	defer cancel()
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
	// The wait for the guest's boot is part of the call, so the bound adds it.
	ctx, cancel := s.accepted(ctx, s.bootTimeout)
	defer cancel()
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

	_, kernel, ok := s.kernels.resolve(m.Kernel)
	if !ok {
		return fail(ErrPreconditionFailed, "kernel %q is not offered by the node", cmp.Or(m.Kernel, s.kernels.Default))
	}
	if err := s.permitter.PermitStart(ctx, id); err != nil {
		return fail(ErrPreconditionFailed, "starting is not permitted: %v", err)
	}
	plan := bootPlan{
		machine: m,
		kernel:  kernel,
		describe: func(ctx context.Context, boot BootSpec) (ProcessDescription, error) {
			return s.driver.Describe(ctx, id, boot)
		},
		vmm: func(ctx context.Context, boot BootSpec) error { return s.driver.Boot(ctx, id, boot) },
		// The guest waits for this description before it does anything else, and
		// Boot returns when the entrypoint runs, so nothing is Running before then.
		// The Machine's lock is held meanwhile, so a repeated Start waits for the
		// outcome of this one.
		guest: func(ctx context.Context, image LocatedImage) error {
			if len(m.Disks) > 0 {
				// A find reaches the agent only once it listens, which Boot
				// waits for itself.
				if err := s.awaitAgent(ctx, id); err != nil {
					return err
				}
			}
			for _, a := range m.Disks {
				if err := s.plugAtStart(ctx, id, a); err != nil {
					return err
				}
			}
			run, user, err := m.Spec.runOn(m.Image, image.Run)
			if err != nil {
				return err
			}
			description := GuestBoot{ImageUUID: image.UUID, RootDiskUUID: m.RootDiskUUID, Run: run, User: user, Files: m.Files, Network: m.Network.guestNetwork(), Clock: s.clock}
			if err := s.guest.Boot(ctx, id, description); err != nil {
				return fmt.Errorf("send the boot description to the guest: %w", err)
			}
			return nil
		},
		guestWhile: "booting",
	}
	res := s.bringUp(ctx, plan)
	if res.failure != nil {
		if !res.begun.vmm {
			// No VMM process began, so the Machine keeps its state.
			return fail(res.failure.Kind, "%s", res.reason())
		}
		return s.bootFailed(ctx, rec, res)
	}

	next := m.clone()
	next.State = StateRunning
	next.BootMemoryBytes = m.Resources.MemoryBytes
	next.KernelDigest = res.kernelDigest
	next.FailureReason = ""
	if err := s.commit(ctx, rec, next); err != nil {
		return s.unrecorded(rec, err)
	}
	return nil
}

// bootDevices lists a Machine's block devices in boot order: its image
// read-only, then its root disk. It returns the image as the node holds it,
// whose UUID and run configuration the guest is told. Every other disk reaches
// a started VMM by hot-plug (ADR 0041).
func (s *service) bootDevices(ctx context.Context, m Machine) ([]BlockDevice, LocatedImage, error) {
	located, err := s.images.Locate(ctx, m.Image)
	if err != nil {
		return nil, LocatedImage{}, locateFailure(err, ErrImageAbsent, "image %s", m.Image)
	}
	root, err := s.diskDevices(ctx, DiskAttachment{Disk: m.RootDisk})
	if err != nil {
		return nil, LocatedImage{}, err
	}
	return append([]BlockDevice{{Path: located.Path, ReadOnly: true}}, root...), located, nil
}

// diskDevices locates the files of disks as the block devices a VMM is given.
func (s *service) diskDevices(ctx context.Context, attached ...DiskAttachment) ([]BlockDevice, error) {
	var devices []BlockDevice
	for _, a := range attached {
		path, err := s.disks.Locate(ctx, a.Disk)
		if err != nil {
			return nil, locateFailure(err, ErrDiskAbsent, "disk %q", a.Disk)
		}
		devices = append(devices, BlockDevice{Disk: a.Disk, Path: path, ReadOnly: a.ReadOnly})
	}
	return devices, nil
}

// agentPoll is how long awaitAgent waits between two asks of a guest that has
// not answered.
const agentPoll = 100 * time.Millisecond

// awaitAgent asks the guest until the agent answers, or until the context ends,
// which is the boot timeout's.
func (s *service) awaitAgent(ctx context.Context, id MachineID) error {
	for {
		err := s.guest.Health(ctx, id)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for the Guest Agent: %w", err)
		case <-time.After(agentPoll):
		}
	}
}

// plugAtStart puts a disk the Machine names on the VMM Start has booted and has
// the guest find it, so the guest records it under its ID (ADR 0041). A failure
// names the disk; the caller ends the VMM.
func (s *service) plugAtStart(ctx context.Context, id MachineID, a DiskAttachment) error {
	devices, err := s.diskDevices(ctx, a)
	if err != nil {
		return err
	}
	if err := s.driver.AttachDisk(ctx, id, devices[0]); err != nil {
		return fmt.Errorf("attach disk %q: %w", a.Disk, err)
	}
	if err := s.guest.FindDisk(ctx, id, a); err != nil {
		return fmt.Errorf("the guest could not find disk %q: %w", a.Disk, err)
	}
	return nil
}

func locateFailure(err, absent error, what string, args ...any) error {
	what = fmt.Sprintf(what, args...)
	if errors.Is(err, absent) {
		return fail(ErrPreconditionFailed, "%s is not on the node", what)
	}
	return fail(ErrFailed, "locate %s: %v", what, err)
}

// bootFailed ends a boot that began and failed, after the pipeline ended the
// VMM: the Machine is Stopped with the reason, or Faulted if the VMM or its
// network could not be removed.
func (s *service) bootFailed(ctx context.Context, rec *Record, boot bootResult) error {
	ctx, cancel := s.compensating(ctx)
	defer cancel()
	next := rec.Machine.clone()
	next.State = StateStopped
	next.FailureReason = "boot failed: " + boot.reason()
	if boot.endErr != nil || boot.withdrawErr != nil {
		next.State = StateFaulted
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
	ctx, cancel := s.accepted(ctx, grace)
	defer cancel()
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
		s.askGuest(ctx, id, grace, m.State == StatePaused)
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

// vmmPoll is how often a stop looks for the end of the VMM.
const vmmPoll = 50 * time.Millisecond

// askGuest asks the guest to stop through its agent, which applies the
// Termination rule whatever the VMM and the guest kernel (ADR 0018), and waits
// up to grace for the VMM to end. The call to the agent is bounded by the
// grace, so nothing here outlasts it. Any outcome of that call but success,
// and the cases where the agent is not asked (no grace, or a Paused guest that
// cannot run it), fall back to the Driver's own request with what is left of
// the grace; a Driver that did not confirm the end is followed by a wait for
// the VMM until the deadline. Nothing here decides the outcome: the caller
// ends the VMM either way.
func (s *service) askGuest(ctx context.Context, id MachineID, grace time.Duration, paused bool) {
	deadline := time.Now().Add(grace)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if grace > 0 && !paused {
		if err := s.guest.Terminate(waitCtx, id); err == nil {
			s.awaitVMMEnd(waitCtx, id)
			return
		}
	}
	if err := s.driver.Shutdown(ctx, id, max(time.Until(deadline), 0)); err != nil {
		s.awaitVMMEnd(waitCtx, id)
	}
}

// awaitVMMEnd returns when the Machine has no supervisor, which ends with its
// VMM, or when ctx ends.
func (s *service) awaitVMMEnd(ctx context.Context, id MachineID) {
	tick := time.NewTicker(vmmPoll)
	defer tick.Stop()
	for {
		if live, err := s.supervisor.Live(ctx); err == nil && !slices.Contains(live, id) {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
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
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
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
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
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
	// The attached disks outlive the Machine and are mvmd's again.
	for _, a := range rec.Machine.Disks {
		if err := s.reclaim(ctx, a.Disk); err != nil {
			return fail(ErrFailed, "%v", err)
		}
	}
	// A Machine restored from an unreadable record has no root disk name:
	// the disk file is left for the operator, since naming one to remove
	// would be a guess.
	if rec.Machine.RootDisk != "" {
		if err := s.disks.Remove(ctx, rec.Machine.RootDisk); err != nil {
			return fail(ErrFailed, "remove the root disk: %v", err)
		}
	}
	// Last of the host's state, so that a failure keeps the record and the
	// Machine's identifier, and a repeat removes the directory again.
	if err := s.supervisor.RemoveState(ctx, id); err != nil {
		return fail(ErrFailed, "%v", err)
	}
	if err := s.store.Remove(ctx, id); err != nil {
		return fail(ErrFailed, "remove the record: %v", err)
	}
	s.mu.Lock()
	delete(s.records, id)
	delete(s.held, rec.Machine.RootDisk)
	for _, a := range rec.Machine.Disks {
		delete(s.held, a.Disk)
	}
	s.uids.ReleaseMachine(id)
	s.mu.Unlock()
	s.dropExecs(id)
	// The Machine's lock is still held, so this follows every event of its
	// states, and a watcher that sees it finds the Machine gone from List.
	s.events.Deleted(id)
	return nil
}
