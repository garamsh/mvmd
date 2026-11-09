package machine

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

func hot(m Machine) bool {
	return m.State == StateRunning || m.State == StatePaused
}

func (s *service) AttachDisk(ctx context.Context, id MachineID, a DiskAttachment) error {
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
	if have, attached := m.attachment(a.Disk); attached {
		if have == a {
			return nil
		}
		return fail(ErrConflict, "disk %q is attached to Machine %q with other parameters", a.Disk, id)
	}
	path, err := s.disks.Locate(ctx, a.Disk)
	if err != nil {
		if errors.Is(err, ErrDiskAbsent) {
			return fail(ErrNotFound, "disk %q is not held by the node", a.Disk)
		}
		return fail(ErrFailed, "locate disk %q: %v", a.Disk, err)
	}
	if m.State == StatePaused {
		return fail(ErrPreconditionFailed, "Machine %q is Paused and its guest cannot find a disk; resume it first", id)
	}
	if hot(m) && !s.driver.Info().has(CapabilityDiskHotplug) {
		return fail(ErrUnsupported, "the Driver cannot attach a disk to a %s Machine", m.State)
	}
	// One holder per disk (ADR 0009): a disk another Machine holds, as writer
	// or as reader, takes no second holder.
	if _, holder, ok := s.claim(id, a.Disk); !ok {
		return fail(ErrPreconditionFailed, "disk %q is held by Machine %q", a.Disk, holder)
	}

	// The holder is recorded durably before a Driver is asked to attach.
	next := m.clone()
	next.Disks = append(next.Disks, a)
	if err := s.commit(ctx, rec, next); err != nil {
		s.release(a.Disk)
		return fail(ErrFailed, "record the holder of disk %q: %v", a.Disk, err)
	}
	if hot(m) {
		dev := BlockDevice{Disk: a.Disk, Path: path, ReadOnly: a.ReadOnly}
		if err := s.driver.AttachDisk(ctx, id, dev); err != nil {
			ctx, cancel := s.compensating(ctx)
			defer cancel()
			if cerr := s.commit(ctx, rec, m); cerr != nil {
				return s.unrecorded(rec, cerr)
			}
			s.release(a.Disk)
			return fail(ErrFailed, "attach disk %q: %v", a.Disk, err)
		}
		// The guest finds the device it is not told of (ADR 0022). A guest
		// that cannot leaves the disk on the VMM and not in the guest, so it
		// is taken off again and the attach has not happened.
		if err := s.guest.FindDisk(ctx, id, a.Disk); err != nil {
			ctx, cancel := s.compensating(ctx)
			defer cancel()
			if derr := s.driver.DetachDisk(ctx, id, a.Disk); derr != nil {
				return s.diverged(rec, "disk %q is on the VMM, the guest could not find it (%v) and the Driver could not detach it: %v", a.Disk, err, derr)
			}
			if cerr := s.commit(ctx, rec, m); cerr != nil {
				return s.unrecorded(rec, cerr)
			}
			s.release(a.Disk)
			return fail(ErrFailed, "the guest could not find disk %q: %v", a.Disk, err)
		}
	}
	return nil
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
				return fail(ErrPreconditionFailed, "the guest of Machine %q did not find disk %q while it ran, so it cannot release it; stop the Machine to detach it", id, disk)
			}
			return fail(ErrFailed, "the guest could not release disk %q: %v", disk, err)
		}
		if err := s.driver.DetachDisk(ctx, id, disk); err != nil {
			// The guest has let go of a disk the VMM still has: ask it to
			// find the disk again, as far as it can.
			ctx, cancel := s.compensating(ctx)
			defer cancel()
			_ = s.guest.FindDisk(ctx, id, disk)
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
	s.release(disk)
	return nil
}

func (s *service) SetResources(ctx context.Context, id MachineID, target Resources) error {
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
	if m.Resources == target {
		return nil
	}
	if hot(m) && !s.driver.Info().has(CapabilityResize) {
		return fail(ErrUnsupported, "the Driver cannot resize a %s Machine", m.State)
	}

	if hot(m) {
		if err := s.driver.Resize(ctx, id, target); err != nil {
			return fail(ErrFailed, "resize: %v", err)
		}
	}
	next := m.clone()
	next.Resources = target
	if err := s.commit(ctx, rec, next); err != nil {
		if hot(m) {
			return s.unrecorded(rec, err)
		}
		return fail(ErrFailed, "record the target: %v", err)
	}
	return nil
}
