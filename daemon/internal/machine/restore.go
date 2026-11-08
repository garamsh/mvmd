package machine

import (
	"context"
	"fmt"
)

const (
	// reasonVMMEnded is the reason of a Machine found without a supervisor.
	reasonVMMEnded = "the VMM ended while mvmd was not running"
)

// Restore rebuilds the lifecycle from the State Store after a restart: the
// records, the request-ID memory and the disk-holder index. It then checks each
// Machine recorded as Running or Paused against the live supervisors. It runs
// once, before any other call.
//
// A VMM runs in its supervisor's PID namespace, which dies with its init
// (ADR 0003), so a Machine with no live supervisor has no VMM: it becomes
// Stopped with the reason, and its network is withdrawn. A Machine whose
// supervisor lives keeps its recorded state. If the supervisors cannot be
// listed, the Machines that claim to run are Faulted, since mvmd cannot
// establish their state; so is a Machine whose record cannot be decoded. The
// Driver's own list of VMMs confirms a Machine in the Driver issues.
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
		s.records[rec.ID] = &rec
		if m := rec.Machine; m != nil && rec.Unreadable == "" {
			s.held[m.RootDisk] = m.ID
			for _, a := range m.Disks {
				s.held[a.Disk] = m.ID
			}
		}
	}
	s.mu.Unlock()

	var claimed []*Record
	for _, rec := range records {
		if m := rec.Machine; rec.Unreadable == "" && m != nil && (m.State == StateRunning || m.State == StatePaused) {
			stored, _ := s.lookup(rec.ID)
			claimed = append(claimed, stored)
		}
	}
	if len(claimed) == 0 {
		return nil
	}
	live := map[MachineID]bool{}
	ids, liveErr := s.supervisor.Live(ctx)
	for _, id := range ids {
		live[id] = true
	}
	for _, stored := range claimed {
		switch {
		case liveErr != nil:
			err = s.settleRestored(ctx, stored, StateFaulted, fmt.Sprintf("the supervisors could not be listed: %v", liveErr))
		case !live[stored.ID]:
			err = s.settleRestored(ctx, stored, StateStopped, reasonVMMEnded)
		}
		if err != nil {
			return err
		}
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
