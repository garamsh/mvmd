package machine

import (
	"context"
	"errors"
	"slices"
)

func hot(m Machine) bool {
	return m.State == StateRunning || m.State == StatePaused
}

func (s *service) AttachDisk(ctx context.Context, id MachineID, a DiskAttachment) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	// One holder per disk (ADR 0009): a disk another Machine holds, as writer
	// or as reader, takes no second holder.
	if holder, held := s.holder(a.Disk); held {
		return fail(ErrPreconditionFailed, "disk %q is held by Machine %q", a.Disk, holder)
	}
	if hot(m) && !s.driver.Info().has(CapabilityDiskHotplug) {
		return fail(ErrUnsupported, "the Driver cannot attach a disk to a %s Machine", m.State)
	}

	// The holder is recorded durably before a Driver is asked to attach.
	next := m.clone()
	next.Disks = append(next.Disks, a)
	if err := s.commit(ctx, rec, next); err != nil {
		return fail(ErrFailed, "record the holder of disk %q: %v", a.Disk, err)
	}
	if hot(m) {
		dev := BlockDevice{Disk: a.Disk, Path: path, ReadOnly: a.ReadOnly}
		if err := s.driver.AttachDisk(ctx, id, dev); err != nil {
			if cerr := s.commit(ctx, rec, m); cerr != nil {
				return s.unrecorded(rec, cerr)
			}
			return fail(ErrFailed, "attach disk %q: %v", a.Disk, err)
		}
	}
	return nil
}

func (s *service) DetachDisk(ctx context.Context, id MachineID, disk DiskID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
	if hot(m) && !s.driver.Info().has(CapabilityDiskHotplug) {
		return fail(ErrUnsupported, "the Driver cannot detach a disk from a %s Machine", m.State)
	}

	// The holder is released only once the VMM no longer has the disk open.
	if hot(m) {
		if err := s.driver.DetachDisk(ctx, id, disk); err != nil {
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
	return nil
}

func (s *service) SetResources(ctx context.Context, id MachineID, target Resources) error {
	s.mu.Lock()
	defer s.mu.Unlock()
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
