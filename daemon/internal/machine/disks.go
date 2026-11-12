package machine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

func hot(m Machine) bool {
	return m.State == StateRunning || m.State == StatePaused
}

// deleting is the holder a disk takes while DeleteDisk removes it, so that no
// attach or create holds a file on its way out.
const deleting MachineID = ""

// invalidDisk is the outcome of a call that names an identifier no disk file
// has, refused before the call takes a lock or reaches a port.
func invalidDisk(disk DiskID) error {
	return fail(ErrInvalid, "%q is not a disk identifier: it must not be empty, ., .., start with %s, contain / or a NUL, or exceed %d bytes", disk, TempDiskPrefix, maxNameBytes)
}

// checkMountPath returns the reason a disk attachment's mount path is refused,
// or nil: empty mounts nothing, and any other path is absolute and clean, not
// the root and not under a directory the guest reserves (guest.md).
func checkMountPath(p string) error {
	switch {
	case p == "":
		return nil
	case !absoluteClean(p):
		return fail(ErrInvalid, "mount path %q is not absolute and clean", p)
	case reservedRunPath(p):
		return fail(ErrInvalid, "mount path %q is under a reserved directory", p)
	}
	return nil
}

// checkMountable returns an error when a mount path is asked for a disk the
// node holds that has no filesystem to mount.
func (s *service) checkMountable(ctx context.Context, a DiskAttachment) error {
	if a.MountPath == "" {
		return nil
	}
	e, err := s.disks.Get(ctx, a.Disk)
	switch {
	case errors.Is(err, ErrDiskAbsent):
		return fail(ErrNotFound, "disk %q is not held by the node", a.Disk)
	case err != nil:
		return fail(ErrFailed, "read disk %q: %v", a.Disk, err)
	case e.Filesystem == FilesystemNone:
		return fail(ErrInvalid, "disk %q has no filesystem, so it cannot be mounted at %q", a.Disk, a.MountPath)
	}
	return nil
}

func (s *service) CreateDisk(ctx context.Context, disk DiskID, request RequestID, sizeBytes uint64, filesystem Filesystem) error {
	if filesystem == "" {
		filesystem = FilesystemExt4
	}
	switch {
	case !ValidDiskID(disk):
		return invalidDisk(disk)
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	case sizeBytes == 0:
		return fail(ErrInvalid, "the disk size is zero")
	case filesystem != FilesystemNone && filesystem != FilesystemExt4:
		return fail(ErrInvalid, "filesystem %q is neither %q nor %q", filesystem, FilesystemNone, FilesystemExt4)
	}
	defer s.diskLocks.lock(disk, true)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	asked := DiskEntry{ID: disk, Request: request, SizeBytes: sizeBytes, Filesystem: filesystem}
	for attempt := 0; ; attempt++ {
		switch have, err := s.disks.Get(ctx, disk); {
		case err == nil:
			if have == asked {
				return nil
			}
			return fail(ErrConflict, "disk %q exists under another request ID, size or filesystem", disk)
		case !errors.Is(err, ErrDiskAbsent):
			return fail(ErrFailed, "look up disk %q: %v", disk, err)
		}
		err := s.disks.Create(ctx, asked)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrDiskInvalid):
			return fail(ErrInvalid, "%v", err)
		case errors.Is(err, ErrDiskExists) && attempt == 0:
			// A call made the disk between the lookup and the create: what it
			// made is what the lookup now answers for.
		default:
			return fail(ErrFailed, "create disk %q: %v", disk, err)
		}
	}
}

// ownerOf is who owns a disk file while the Machine holds the disk: the user
// its VMM runs as.
func ownerOf(m Machine) DiskOwner {
	return DiskOwner{ID: m.ID, UID: m.User.UID, GID: m.User.GID}
}

// give makes the files of the disks the owner's, all or none: a failure gives
// back the ones already given and reports what could not be undone.
func (s *service) give(ctx context.Context, owner DiskOwner, disks ...DiskID) error {
	for i, disk := range disks {
		if err := s.disks.Give(ctx, disk, owner); err != nil {
			ctx, cancel := s.compensating(ctx)
			defer cancel()
			return errors.Join(fmt.Errorf("give disk %q to Machine %q: %w", disk, owner.ID, err), s.reclaim(ctx, disks[:i]...))
		}
	}
	return nil
}

// reclaim makes the files of the disks mvmd's again. A disk the node no longer
// holds has no file to give back.
func (s *service) reclaim(ctx context.Context, disks ...DiskID) error {
	var errs []error
	for _, disk := range disks {
		if err := s.disks.Reclaim(ctx, disk); err != nil && !errors.Is(err, ErrDiskAbsent) {
			errs = append(errs, fmt.Errorf("give disk %q back: %w", disk, err))
		}
	}
	return errors.Join(errs...)
}

// giveBack ends a Machine's hold on the disks: the files are mvmd's again and
// the holder slots are free, whether or not the first succeeded.
func (s *service) giveBack(ctx context.Context, disks ...DiskID) error {
	defer s.release(disks...)
	return s.reclaim(ctx, disks...)
}

func (s *service) ListDisks(ctx context.Context) ([]DiskInfo, error) {
	entries, err := s.disks.List(ctx)
	if err != nil {
		return nil, fail(ErrFailed, "list the disks: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]DiskInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, DiskInfo{ID: e.ID, SizeBytes: e.SizeBytes, Filesystem: e.Filesystem, Holder: s.held[e.ID]})
	}
	return out, nil
}

func (s *service) DeleteDisk(ctx context.Context, disk DiskID) error {
	if !ValidDiskID(disk) {
		return invalidDisk(disk)
	}
	// Taking the holder slot refuses a disk with a holder and keeps an attach
	// from holding the file while it is removed.
	if _, holder, ok := s.claim(deleting, disk); !ok {
		if holder == deleting {
			return fail(ErrPreconditionFailed, "disk %q is being deleted by another call", disk)
		}
		return fail(ErrPreconditionFailed, "disk %q is held by Machine %q", disk, holder)
	}
	defer s.release(disk)
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	if err := s.disks.Remove(ctx, disk); err != nil {
		return fail(ErrFailed, "remove disk %q: %v", disk, err)
	}
	return nil
}

func (s *service) AttachDisk(ctx context.Context, id MachineID, a DiskAttachment) error {
	if !ValidDiskID(a.Disk) {
		return invalidDisk(a.Disk)
	}
	defer s.lock(id)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	m := *rec.Machine
	if err := s.requireNotFaulted(m, "attach"); err != nil {
		return err
	}
	if err := checkMountPath(a.MountPath); err != nil {
		return err
	}
	if a.MountPath != "" {
		for _, other := range m.Disks {
			if false && other.Disk != a.Disk && other.MountPath == a.MountPath {
				return fail(ErrInvalid, "mount path %q is already the mount of disk %q", a.MountPath, other.Disk)
			}
		}
	}
	if have, attached := m.attachment(a.Disk); attached {
		if have != a {
			return fail(ErrConflict, "disk %q is attached to Machine %q with other parameters", a.Disk, id)
		}
		return s.converge(ctx, rec, a)
	}
	path, err := s.locate(ctx, a.Disk)
	if err != nil {
		return err
	}
	if err := s.checkMountable(ctx, a); err != nil {
		return err
	}
	if m.State == StatePaused {
		return fail(ErrPreconditionFailed, "Machine %q is Paused and its guest cannot find a disk; resume it first", id)
	}
	// Start plugs every attached disk (ADR 0041), so a Driver that cannot
	// hot-plug takes no attach in any state: it would fail every later Start.
	if !s.driver.Info().has(CapabilityDiskHotplug) {
		return fail(ErrUnsupported, "the Driver cannot hot-plug, so it cannot attach a disk to a %s Machine", m.State)
	}
	// One holder per disk (ADR 0009): a disk another Machine holds, as writer
	// or as reader, takes no second holder.
	if _, holder, ok := s.claim(id, a.Disk); !ok {
		if holder == deleting {
			return fail(ErrPreconditionFailed, "disk %q is being deleted", a.Disk)
		}
		return fail(ErrPreconditionFailed, "disk %q is held by Machine %q", a.Disk, holder)
	}

	// The file is the holder's before a Driver is asked to open it (ADR 0038).
	if err := s.give(ctx, ownerOf(m), a.Disk); err != nil {
		s.release(a.Disk)
		return fail(ErrFailed, "%v", err)
	}

	// The holder is recorded durably before a Driver is asked to attach.
	next := m.clone()
	next.Disks = append(next.Disks, a)
	if err := s.commit(ctx, rec, next); err != nil {
		return fail(ErrFailed, "record the holder of disk %q: %v", a.Disk, errors.Join(err, s.giveBack(ctx, a.Disk)))
	}
	if hot(m) {
		return s.plug(ctx, rec, a, BlockDevice{Disk: a.Disk, Path: path, ReadOnly: a.ReadOnly}, m)
	}
	return nil
}

// locate returns the path of the disk the node holds, as the outcome a call
// answers with when it cannot.
func (s *service) locate(ctx context.Context, disk DiskID) (string, error) {
	path, err := s.disks.Locate(ctx, disk)
	switch {
	case errors.Is(err, ErrDiskAbsent):
		return "", fail(ErrNotFound, "disk %q is not held by the node", disk)
	case err != nil:
		return "", fail(ErrFailed, "locate disk %q: %v", disk, err)
	}
	return path, nil
}

// plug puts a disk the record already lists on the running VMM and has the
// guest find it. A step that fails is undone, and the record is set back to
// without, the Machine as it was with no such disk, so the attach has not
// happened.
func (s *service) plug(ctx context.Context, rec *Record, a DiskAttachment, dev BlockDevice, without Machine) error {
	id, disk := rec.ID, dev.Disk
	if err := s.driver.AttachDisk(ctx, id, dev); err != nil {
		ctx, cancel := s.compensating(ctx)
		defer cancel()
		if cerr := s.commit(ctx, rec, without); cerr != nil {
			return s.unrecorded(rec, cerr)
		}
		return fail(ErrFailed, "attach disk %q: %v", disk, errors.Join(err, s.giveBack(ctx, disk)))
	}
	// The guest finds the device it is not told of (ADR 0022). A guest
	// that cannot leaves the disk on the VMM and not in the guest, so it
	// is taken off again and the attach has not happened.
	if err := s.guest.FindDisk(ctx, id, a); err != nil {
		ctx, cancel := s.compensating(ctx)
		defer cancel()
		if derr := s.driver.DetachDisk(ctx, id, disk); derr != nil {
			return s.diverged(rec, "disk %q is on the VMM, the guest could not find it (%v) and the Driver could not detach it: %v", disk, err, derr)
		}
		if cerr := s.commit(ctx, rec, without); cerr != nil {
			return s.unrecorded(rec, cerr)
		}
		return fail(ErrFailed, "the guest could not find disk %q: %v", disk, errors.Join(err, s.giveBack(ctx, disk)))
	}
	return nil
}

// converge answers an attach of a disk the record already lists. On a Running
// Machine the record is not proof that the attach that wrote it finished
// (ADR 0023), so the VMM is asked: a drive it has had since boot is the
// guest's own, one added since is found by the guest again, and where there is
// none the disk is attached now, as the original call would have.
func (s *service) converge(ctx context.Context, rec *Record, a DiskAttachment) error {
	m := *rec.Machine
	if m.State != StateRunning || !s.driver.Info().has(CapabilityDiskHotplug) {
		return nil
	}
	state, err := s.driver.Drive(ctx, m.ID, a.Disk)
	if err != nil {
		return fail(ErrFailed, "read the drive of disk %q from the VMM of Machine %q: %v", a.Disk, m.ID, err)
	}
	switch state {
	case DriveAtBoot:
		return nil
	case DriveHotPlugged:
		if err := s.guest.FindDisk(ctx, m.ID, a); err != nil {
			return fail(ErrFailed, "the guest of Machine %q could not find disk %q, which the VMM has: %v", m.ID, a.Disk, err)
		}
		return nil
	}
	path, err := s.locate(ctx, a.Disk)
	if err != nil {
		return err
	}
	without := m.clone()
	without.Disks = slices.DeleteFunc(without.Disks, func(have DiskAttachment) bool { return have.Disk == a.Disk })
	return s.plug(ctx, rec, a, BlockDevice{Disk: a.Disk, Path: path, ReadOnly: a.ReadOnly}, without)
}

// diverged marks the Machine Faulted when the VMM and the guest disagree about
// a disk and mvmd could not bring them back together.
func (s *service) diverged(rec *Record, format string, args ...any) error {
	faulted := rec.Machine.clone()
	faulted.State = StateFaulted
	faulted.FailureReason = fmt.Sprintf(format, args...)
	s.setMachine(rec, &faulted)
	return fail(ErrFailed, "%s", faulted.FailureReason)
}

func (s *service) DetachDisk(ctx context.Context, id MachineID, disk DiskID) error {
	if !ValidDiskID(disk) {
		return invalidDisk(disk)
	}
	defer s.lock(id)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	m := *rec.Machine
	if err := s.requireNotFaulted(m, "detach"); err != nil {
		return err
	}
	if _, attached := m.attachment(disk); !attached {
		if m.RootDisk == disk {
			return fail(ErrPreconditionFailed, "disk %q is the root disk of Machine %q and is held for its life", disk, id)
		}
		return nil
	}
	if m.State == StatePaused {
		return fail(ErrPreconditionFailed, "Machine %q is Paused and its guest cannot release a disk; resume it first", id)
	}
	if hot(m) && !s.driver.Info().has(CapabilityDiskHotplug) {
		return fail(ErrUnsupported, "the Driver cannot detach a disk from a %s Machine", m.State)
	}

	// The guest lets go of the device first (ADR 0022), and the holder is
	// released only once the VMM no longer has the disk open. A guest that
	// refuses or does not answer leaves the disk attached.
	if hot(m) {
		if err := s.guest.ReleaseDisk(ctx, id, disk); err != nil {
			switch {
			case errors.Is(err, ErrDiskInUse):
				return fail(ErrPreconditionFailed, "the guest of Machine %q is using disk %q: %v", id, disk, err)
			case errors.Is(err, ErrDiskUnknown):
				return fail(ErrPreconditionFailed, "the guest of Machine %q never found disk %q while it ran, so it cannot release it; stop the Machine to detach it", id, disk)
			}
			return fail(ErrFailed, "the guest could not release disk %q: %v", disk, err)
		}
		if err := s.driver.DetachDisk(ctx, id, disk); err != nil {
			// The guest has let go of a disk the VMM still has: ask it to
			// find the disk again, as far as it can. If it cannot, a retried
			// Detach releases again without effect and tries the Driver once
			// more.
			ctx, cancel := s.compensating(ctx)
			defer cancel()
			att, _ := m.attachment(disk)
			_ = s.guest.FindDisk(ctx, id, att)
			return fail(ErrFailed, "detach disk %q: %v", disk, err)
		}
	}
	next := m.clone()
	next.Disks = slices.DeleteFunc(next.Disks, func(a DiskAttachment) bool { return a.Disk == disk })
	if err := s.commit(ctx, rec, next); err != nil {
		if hot(m) {
			return s.unrecorded(rec, err)
		}
		return fail(ErrFailed, "record the release of disk %q: %v", disk, err)
	}
	if err := s.giveBack(ctx, disk); err != nil {
		return fail(ErrFailed, "disk %q is detached and could not be given back to mvmd: %v", disk, err)
	}
	return nil
}

func (s *service) SetResources(ctx context.Context, id MachineID, target, ceiling Resources) error {
	defer s.lock(id)()
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()
	rec, err := s.machine(id)
	if err != nil {
		return err
	}
	m := *rec.Machine
	if err := s.requireNotFaulted(m, "set resources"); err != nil {
		return err
	}
	if target.VCPUs == 0 || target.MemoryBytes == 0 {
		return fail(ErrInvalid, "the target needs at least one vCPU and some memory")
	}
	// A resource the Driver cannot resize holds a ceiling figure equal to the
	// resources', so a figure left unset follows the target of a Machine that
	// is not hot (a hot one is refused below); a resource it can resize keeps
	// the held figure.
	info := s.driver.Info()
	held := ceiling.OrElse(m.Ceiling)
	if !hot(m) {
		follow := ceiling.OrElse(target)
		if !info.has(CapabilityCPUResize) {
			held.VCPUs = follow.VCPUs
		}
		if !info.has(CapabilityMemoryResize) {
			held.MemoryBytes = follow.MemoryBytes
		}
	}
	if err := s.checkCeiling(target, held, ceiling != Resources{}); err != nil {
		return err
	}
	if hot(m) && held != m.Ceiling {
		return fail(ErrPreconditionFailed, "the ceiling of a %s Machine does not change; stop the Machine to change it", m.State)
	}
	if m.Resources == target && held == m.Ceiling {
		return nil
	}
	if hot(m) && target.VCPUs != m.Resources.VCPUs && !info.has(CapabilityCPUResize) {
		return fail(ErrUnsupported, "the Driver cannot resize the vCPUs of a %s Machine", m.State)
	}
	if hot(m) && target.MemoryBytes != m.Resources.MemoryBytes && !info.has(CapabilityMemoryResize) {
		return fail(ErrUnsupported, "the Driver cannot resize the memory of a %s Machine", m.State)
	}
	if hot(m) && target.MemoryBytes < m.BootMemoryBytes {
		return fail(ErrPreconditionFailed, "a running VMM reaches no less memory than it booted with, %d bytes; stop the Machine to apply %d", m.BootMemoryBytes, target.MemoryBytes)
	}

	if m.State == StatePaused {
		return fail(ErrPreconditionFailed, "Machine %q is Paused and its guest cannot take part in a resize; resume it first", id)
	}

	if hot(m) {
		if err := s.driver.Resize(ctx, id, target); err != nil {
			return fail(ErrFailed, "resize: %v", err)
		}
		// The VMM has presented the vCPUs; the guest brings them online
		// (ADR 0029), as it finds a disk the Driver has plugged (ADR 0022).
		// A guest that fails leaves the record at the old target, and a
		// retried resize converges: the Driver sends nothing for what the VMM
		// already holds, and the agent repeats safely.
		if err := s.awaitVcpusOnline(ctx, id, target.VCPUs); err != nil {
			return fail(ErrFailed, "the guest of Machine %q did not bring %d vCPUs online: %v", id, target.VCPUs, err)
		}
	}
	next := m.clone()
	next.Resources = target
	next.Ceiling = held
	if err := s.commit(ctx, rec, next); err != nil {
		if hot(m) {
			return s.unrecorded(rec, err)
		}
		return fail(ErrFailed, "record the target: %v", err)
	}
	return nil
}

// resizePoll is how often awaitVcpusOnline asks the guest again for a count the
// VMM has not finished presenting.
const resizePoll = 50 * time.Millisecond

// awaitVcpusOnline asks the guest to bring vcpus online until it reports that
// many, or the resize timeout passes. A count below the target is the VMM's
// hot-plug not yet seen by the guest, not a failure; an error of the guest is.
func (s *service) awaitVcpusOnline(ctx context.Context, id MachineID, vcpus uint32) error {
	ctx, cancel := context.WithTimeout(ctx, s.resizeTimeout)
	defer cancel()
	for {
		size, err := s.guest.OnlineVcpus(ctx, id, vcpus)
		if err != nil {
			return err
		}
		if size.VCPUs == vcpus {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("%d are online: %w", size.VCPUs, ctx.Err())
		case <-time.After(resizePoll):
		}
	}
}

// overLimits refuses resources or a ceiling above what the Driver accepts of a
// Machine (ADR 0040), as kind: ErrInvalid where the request carries the
// figures, ErrPreconditionFailed where a held snapshot does.
func (s *service) overLimits(resources, ceiling Resources, kind error) error {
	most := s.driver.Info().Limits.MaxVCPUs
	switch {
	case most == 0:
	case resources.VCPUs > most:
		return fail(kind, "%d vCPUs is above the %d the Driver accepts", resources.VCPUs, most)
	case ceiling.VCPUs > most:
		return fail(kind, "a ceiling of %d vCPUs is above the %d the Driver accepts", ceiling.VCPUs, most)
	}
	return nil
}

// checkCeiling refuses resources and a ceiling that break ADR 0029's rules:
// a memory figure off the alignment, resources above the ceiling, or a figure
// above the Driver's limits, is ErrInvalid; a ceiling asked for above the
// resources, in a resource the Driver cannot resize, is ErrUnsupported, as it
// could never be used.
func (s *service) checkCeiling(resources, ceiling Resources, asked bool) error {
	if err := s.overLimits(resources, ceiling, ErrInvalid); err != nil {
		return err
	}
	switch {
	case resources.MemoryBytes%MemoryAlignment != 0:
		return fail(ErrInvalid, "the memory %d is not a multiple of %d bytes", resources.MemoryBytes, MemoryAlignment)
	case ceiling.MemoryBytes%MemoryAlignment != 0:
		return fail(ErrInvalid, "the memory ceiling %d is not a multiple of %d bytes", ceiling.MemoryBytes, MemoryAlignment)
	case !resources.within(ceiling):
		return fail(ErrInvalid, "the resources %+v are above the ceiling %+v", resources, ceiling)
	case asked && ceiling.VCPUs != resources.VCPUs && !s.driver.Info().has(CapabilityCPUResize):
		return fail(ErrUnsupported, "the Driver cannot resize the vCPUs, so a ceiling above them could never be used")
	case asked && ceiling.MemoryBytes != resources.MemoryBytes && !s.driver.Info().has(CapabilityMemoryResize):
		return fail(ErrUnsupported, "the Driver cannot resize the memory, so a ceiling above it could never be used")
	}
	return nil
}
