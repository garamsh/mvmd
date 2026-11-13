package machine

import (
	"context"
	"errors"
	"fmt"
	"slices"
)

const (
	// reasonVMMEnded begins the reason of a Machine found without a supervisor.
	reasonVMMEnded = "the VMM ended while mvmd was not running"
	// reasonNetworkLost is the reason of a Machine whose network the node no
	// longer holds.
	reasonNetworkLost = "the Machine's network could not be restored after mvmd restarted"
)

// Restore rebuilds the lifecycle from the State Store after a restart: the
// records, the request-ID memory, the exec entries and the disk-holder index. It then checks each
// Machine recorded as Running or Paused against the live supervisors. It runs
// once, before any other call.
//
// A VMM runs in its supervisor's PID namespace, which dies with its init
// (ADR 0003), so a Machine with no live supervisor has no VMM: it becomes
// Stopped with the reason, and its network is withdrawn. A Machine whose
// supervisor lives keeps its recorded state. If the supervisors cannot be
// listed, the Machines that claim to run are Faulted, since mvmd cannot
// establish their state; so is a Machine whose record cannot be decoded. A
// live supervisor that no Running or Paused record claims is terminated: its
// Start never reported success. The
// Driver's own list of VMMs confirms a Machine in the Driver issues.
//
// A disk a Running Machine's record lists is checked against the VMM and the
// guest, since the call that recorded it may have died before either had it
// (ADR 0023): reconcileDisks.
func (s *service) Restore(ctx context.Context) error {
	records, err := s.store.Load(ctx)
	if err != nil {
		return fmt.Errorf("load the records: %w", err)
	}
	s.mu.Lock()
	for i := range records {
		rec := records[i]
		if rec.Unreadable != "" {
			// Held in memory only: the stored bytes stay as they are.
			rec.Machine = &Machine{
				ID:            rec.ID,
				State:         StateFaulted,
				FailureReason: "the Machine's record could not be read: " + rec.Unreadable,
			}
		}
		s.execs.load(rec.Execs)
		rec.Execs = nil // the entries are held in the ledger
		s.records[rec.ID] = &rec
		if m := rec.Machine; m != nil && rec.Unreadable == "" {
			s.held[m.RootDisk] = m.ID
			for _, a := range m.Disks {
				s.held[a.Disk] = m.ID
			}
		}
	}
	// Rebuild the allocator from every restored record so that a restarted daemon
	// does not hand out a UID still in use (ADR 0015).
	for i := range records {
		rec := records[i]
		if rec.Unreadable != "" || rec.Machine == nil {
			continue
		}
		if err := s.uids.restoreMachine(rec.ID, rec.Machine.User.UID); err != nil {
			s.mu.Unlock()
			return err
		}
	}
	s.mu.Unlock()

	live := map[MachineID]bool{}
	ids, liveErr := s.supervisor.Live(ctx)
	for _, id := range ids {
		live[id] = true
	}
	for _, rec := range records {
		if m := rec.Machine; rec.Unreadable == "" && m != nil && (m.State == StateRunning || m.State == StatePaused) {
			stored, _ := s.lookup(rec.ID)
			switch {
			case liveErr != nil:
				err = s.settleRestored(ctx, stored, StateFaulted, fmt.Sprintf("the supervisors could not be listed: %v", liveErr))
			case !live[rec.ID]:
				err = s.settleRestored(ctx, stored, StateStopped, reasonVMMEnded+": "+s.endCause(ctx, rec.ID))
			}
			if err != nil {
				return err
			}
		}
	}

	// A supervisor no Running or Paused record claims was started by a call
	// that never reported success, or belongs to a Machine whose record is
	// gone: nothing promised it, and no call could reach it.
	var errs []error
	for _, id := range ids {
		stored, recorded := s.lookup(id)
		if recorded && stored.Unreadable == "" && stored.Machine != nil && (stored.Machine.State == StateRunning || stored.Machine.State == StatePaused) {
			continue
		}
		if recorded && stored.Unreadable != "" {
			// Its state is unknown and it is Faulted; Delete ends its VMM.
			continue
		}
		if err := s.endUnclaimed(ctx, id, stored, recorded); err != nil {
			errs = append(errs, err)
		}
	}
	s.refollowProbes()
	return errors.Join(append(errs, s.sweepState(ctx), s.reconcileDisks(ctx), s.reconcileOwners(ctx), s.reconcileNetwork(ctx))...)
}

// sweepState removes the state kept for each Machine the State Store does not
// hold, so that no state outlives the Machine it belonged to. A supervisor
// that still lives was ended above; one that cannot be ended is reported.
func (s *service) sweepState(ctx context.Context) error {
	ids, err := s.supervisor.ListState(ctx)
	if err != nil {
		return fmt.Errorf("list the state kept on the host: %w", err)
	}
	var errs []error
	for _, id := range ids {
		if _, recorded := s.lookup(id); recorded {
			continue
		}
		if err := s.supervisor.RemoveState(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("remove the state of unrecorded Machine %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// reconcileDisks brings each Running Machine's record of its disks into
// agreement with its VMM and its guest, or Faults the Machine with a reason
// naming the disk. A disk the VMM has no drive for was never attached, or was
// unplugged before the record caught up, so the record drops it. A drive added
// since boot is found by the guest again, which is repeatable. Nothing is
// unplugged: the guest may be using the disk, and only its release makes that
// safe. A Driver without disk hot-plug has had no disk added to a running VMM.
func (s *service) reconcileDisks(ctx context.Context) error {
	if !s.driver.Info().has(CapabilityDiskHotplug) {
		return nil
	}
	s.mu.Lock()
	var running []*Record
	for _, rec := range s.records {
		if m := rec.Machine; m != nil && rec.Unreadable == "" && m.State == StateRunning {
			running = append(running, rec)
		}
	}
	s.mu.Unlock()

	var errs []error
	for _, rec := range running {
		for _, a := range slices.Clone(rec.Machine.Disks) {
			if rec.Machine.State != StateRunning {
				break
			}
			if err := s.settleDisk(ctx, rec, a); err != nil {
				errs = append(errs, err)
			}
		}
	}
	return errors.Join(errs...)
}

// settleDisk checks one disk of a Running Machine's record against its VMM and
// its guest.
func (s *service) settleDisk(ctx context.Context, rec *Record, att DiskAttachment) error {
	disk := att.Disk
	state, err := s.driver.Drive(ctx, rec.ID, disk)
	if err != nil {
		return s.faultRestored(ctx, rec, "disk %q: the drives of the VMM could not be read after mvmd restarted: %v", disk, err)
	}
	switch state {
	case DriveHotPlugged:
		if err := s.guest.FindDisk(ctx, rec.ID, att); err != nil {
			return s.faultRestored(ctx, rec, "disk %q is on the VMM and the guest could not find it after mvmd restarted: %v", disk, err)
		}
	case DriveAbsent:
		next := rec.Machine.clone()
		next.Disks = slices.DeleteFunc(next.Disks, func(a DiskAttachment) bool { return a.Disk == disk })
		err := s.commit(ctx, rec, next)
		if err != nil {
			// The record could not be updated: hold the finding in memory.
			s.setMachine(rec, &next)
		}
		var recorded error
		if err != nil {
			recorded = fmt.Errorf("record the restored disks of %q: %w", rec.ID, err)
		}
		return errors.Join(recorded, s.giveBack(ctx, disk))
	}
	return nil
}

// reconcileOwners gives each disk a record lists to its Machine again, so a
// disk file is owned by its holder whatever a death left (ADR 0038). A disk the
// node no longer holds is skipped.
func (s *service) reconcileOwners(ctx context.Context) error {
	s.mu.Lock()
	var machines []Machine
	for _, rec := range s.records {
		if m := rec.Machine; m != nil && rec.Unreadable == "" && len(m.Disks) > 0 {
			machines = append(machines, m.clone())
		}
	}
	s.mu.Unlock()

	var errs []error
	for _, m := range machines {
		for _, a := range m.Disks {
			if err := s.disks.Give(ctx, a.Disk, ownerOf(m)); err != nil && !errors.Is(err, ErrDiskAbsent) {
				errs = append(errs, fmt.Errorf("give disk %q to Machine %q: %w", a.Disk, m.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}

// faultRestored records a Running Machine as Faulted with the reason.
func (s *service) faultRestored(ctx context.Context, rec *Record, format string, args ...any) error {
	next := rec.Machine.clone()
	next.State, next.FailureReason = StateFaulted, fmt.Sprintf(format, args...)
	if err := s.commit(ctx, rec, next); err != nil {
		s.setMachine(rec, &next)
		return fmt.Errorf("record the restored state of %q: %w", rec.ID, err)
	}
	return nil
}

// reconcileNetwork brings the node's network, which outlives mvmd, to what the
// records now say. It runs after the supervisors are settled, so a Machine
// with no VMM is no longer Running or Paused and holds no tap. A Running or
// Paused Machine whose network the port could not restore cannot be vouched
// for, so it is Faulted, and the daemon goes on to serve the rest.
func (s *service) reconcileNetwork(ctx context.Context) error {
	s.mu.Lock()
	var machines []Machine
	for _, rec := range s.records {
		if rec.Machine != nil && rec.Unreadable == "" {
			machines = append(machines, rec.Machine.clone())
		}
	}
	s.mu.Unlock()

	lost, err := s.network.Reconcile(ctx, machines)
	if err != nil {
		return fmt.Errorf("reconcile the network: %w", err)
	}
	var errs []error
	for _, id := range lost {
		rec, ok := s.lookup(id)
		if !ok || rec.Machine == nil || (rec.Machine.State != StateRunning && rec.Machine.State != StatePaused) {
			continue
		}
		next := rec.Machine.clone()
		next.State, next.FailureReason = StateFaulted, reasonNetworkLost
		if err := s.commit(ctx, rec, next); err != nil {
			s.setMachine(rec, &next)
			errs = append(errs, fmt.Errorf("record the restored state of %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// endUnclaimed terminates the supervisor of a Machine the records do not say is
// running and withdraws its network. A recorded Machine whose VMM cannot be
// ended, or whose network cannot be removed, is Faulted; an unrecorded one is
// an error.
func (s *service) endUnclaimed(ctx context.Context, id MachineID, rec *Record, recorded bool) error {
	failure := ""
	var cause error
	if err := s.supervisor.Terminate(ctx, id); err != nil {
		cause = err
		failure = fmt.Sprintf("a supervisor that no record claimed could not be ended: %v", err)
	} else if recorded && rec.Machine != nil {
		if err := s.network.Withdraw(ctx, id); err != nil {
			cause = err
			failure = fmt.Sprintf("a supervisor that no record claimed was ended and the network could not be removed: %v", err)
		}
	}
	switch {
	case cause == nil:
		return nil
	case !recorded || rec.Machine == nil:
		return fmt.Errorf("end the unclaimed supervisor of %q: %w", id, cause)
	}
	next := rec.Machine.clone()
	next.State, next.FailureReason = StateFaulted, failure
	if err := s.commit(ctx, rec, next); err != nil {
		s.setMachine(rec, &next)
		return fmt.Errorf("record the restored state of %q: %w", id, err)
	}
	return nil
}

// settleRestored records the state a restored Machine was found in. A VMM that
// has ended no longer holds its tap, so it goes with the state change.
func (s *service) settleRestored(ctx context.Context, rec *Record, state State, reason string) error {
	next := rec.Machine.clone()
	next.State, next.FailureReason = state, reason
	if state == StateStopped {
		if err := s.network.Withdraw(ctx, rec.ID); err != nil {
			next.State = StateFaulted
			next.FailureReason += fmt.Sprintf("; the network could not be removed: %v", err)
		}
	}
	if err := s.commit(ctx, rec, next); err != nil {
		// The record could not be updated: hold the finding in memory.
		s.setMachine(rec, &next)
		return fmt.Errorf("record the restored state of %q: %w", rec.ID, err)
	}
	return nil
}
