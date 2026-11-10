package machine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
)

// Watch looks for the end of each Running or Paused Machine's VMM every
// vmmEndPoll, until ctx ends. The supervisor exits with its VMM (ADR 0003), so
// a Machine with no supervisor has no VMM.
func (s *service) Watch(ctx context.Context) {
	tick := time.NewTicker(s.vmmEndPoll)
	defer tick.Stop()
	for {
		s.observeEnds(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// observeEnds settles each Machine recorded as Running or Paused that the
// supervisors no longer list. A listing that fails observes nothing; the next
// one tries again.
func (s *service) observeEnds(ctx context.Context) {
	s.mu.Lock()
	var running []MachineID
	for id, rec := range s.records {
		if m := rec.Machine; m != nil && rec.Unreadable == "" && (m.State == StateRunning || m.State == StatePaused) {
			running = append(running, id)
		}
	}
	s.mu.Unlock()
	if len(running) == 0 {
		return
	}
	live, err := s.supervisor.Live(ctx)
	if err != nil {
		return
	}
	for _, id := range running {
		if !slices.Contains(live, id) {
			s.settleEnd(ctx, id)
		}
	}
}

// settleEnd moves the Machine to Stopped when its VMM has ended and no call
// holds it. A call in progress owns the Machine: whatever the VMM's end does to
// that call is the call's outcome, so a Machine a call holds is left, and
// found again by a later look. A Machine the fencing deadline no longer
// permits to start is left to the expiry's ForceStop. The listing that named the Machine is older than
// the lock, so the end is established again once the Machine is held.
func (s *service) settleEnd(ctx context.Context, id MachineID) {
	unlock, ok := s.tryLock(id)
	if !ok {
		return
	}
	defer unlock()
	rec, err := s.machine(id)
	if err != nil {
		return
	}
	m := *rec.Machine
	if m.State != StateRunning && m.State != StatePaused {
		return
	}
	if live, err := s.supervisor.Live(ctx); err != nil || slices.Contains(live, id) {
		return
	}
	// A Machine whose start the fencing deadline refuses is not one the VMM
	// ended: the expiry kills the supervisors, releases its ordering and only
	// then force-stops the Machines, so the end in that gap is the expiry's,
	// and its ForceStop settles the Machine (ADR 0033).
	if err := s.permitter.PermitStart(ctx, id); err != nil {
		return
	}
	ctx, cancel := s.accepted(ctx, 0)
	defer cancel()

	reason := reasonVMMEndedOnItsOwn + ": " + s.endCause(ctx, id)
	next := m.clone()
	next.State, next.FailureReason = StateStopped, reason
	if err := s.network.Withdraw(ctx, id); err != nil {
		next.State = StateFaulted
		next.FailureReason = fmt.Sprintf("%s; the network could not be removed: %v", reason, err)
	}
	if err := s.commit(ctx, rec, next); err != nil {
		s.unrecorded(rec, err)
	}
}

// reasonVMMEndedOnItsOwn begins the reason of a Machine whose VMM ended with no
// call asking it to.
const reasonVMMEndedOnItsOwn = "the VMM ended with no call asking it to"

// endCause says why a VMM that has ended did, from the lines the Guest Agent
// wrote to the console (guest.md §PID 1 duties, "The console lines"). A VMM
// that ended with no such line is reported as that.
func (s *service) endCause(ctx context.Context, id MachineID) string {
	record, found, err := s.entrypointFromLog(ctx, id)
	switch {
	case errors.Is(err, ErrConsoleUnavailable):
		return "the Machine has no console log"
	case err != nil:
		return fmt.Sprintf("its console log could not be read: %v", err)
	case !found:
		return "its console log has no line of the Guest Agent about the entrypoint's end, as when the VMM was killed or crashed"
	}
	switch record.Status {
	case ExecExited:
		if record.TermSignal != 0 {
			return fmt.Sprintf("the entrypoint was ended by signal %d", record.TermSignal)
		}
		return fmt.Sprintf("the entrypoint exited with code %d", record.ExitCode)
	case ExecNotStarted:
		return "the entrypoint did not start: " + record.Reason
	case ExecCutShort:
		if strings.HasPrefix(record.Reason, "the Guest Agent ended: ") {
			return record.Reason
		}
		return "the entrypoint ended: " + record.Reason
	}
	return "its console log says the entrypoint's end in a form mvmd does not know"
}
