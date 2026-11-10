package fencing

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func (s *service) Renew(ctx context.Context, length time.Duration, number uint64) (Status, error) {
	if length <= 0 {
		return Status{}, machine.NewError(machine.ErrInvalid, "the deadline length %s is not positive", length)
	}
	s.order.Lock()
	defer s.order.Unlock()
	rec, stopped, machines := s.current()
	if machines == nil {
		return Status{}, machine.NewError(machine.ErrPreconditionFailed, "the fencing deadline has not been reconciled")
	}
	now := s.clock.Now()

	if rec != nil && number <= rec.Renewal {
		// Not greater: nothing changes. A supervisor that missed the recorded
		// expiry of an earlier attempt is brought to it, so that the answer
		// is true.
		if now < rec.Expiry {
			if err := s.converge(ctx, rec.Expiry); err != nil {
				return Status{}, err
			}
		}
		return status(rec, stopped, now), nil
	}

	event := EventRenewed
	if rec == nil || now >= rec.Expiry {
		event = EventArmed
	}
	next := &Record{Expiry: now.Add(length), BootID: s.clock.BootID(), Length: length, Renewal: number}
	live, err := s.sup.Live(ctx)
	if err != nil {
		return Status{}, machine.NewError(machine.ErrFailed, "list the supervisors: %v", err)
	}

	var killed []machine.MachineID
	if rec != nil && now < rec.Expiry && next.Expiry < rec.Expiry {
		// A shortening goes to the supervisors first, so that none ever holds
		// a later expiry than the record; one that will not take it is killed.
		for _, sv := range live {
			if err := error(nil); err == nil {
				continue
			}
			if err := s.sup.Terminate(ctx, sv.Machine); err != nil {
				return Status{}, machine.NewError(machine.ErrFailed,
					"supervisor of %q neither took the shorter expiry nor could be killed: %v", sv.Machine, err)
			}
			killed = append(killed, sv.Machine)
		}
		if err := s.store.Save(ctx, *next); err != nil {
			return Status{}, machine.NewError(machine.ErrFailed, "record the renewal: %v", err)
		}
		s.set(next, false, event)
	} else {
		// An extension is recorded first and passed on second, so that no
		// supervisor ever holds an expiry the record does not.
		if err := s.store.Save(ctx, *next); err != nil {
			return Status{}, machine.NewError(machine.ErrFailed, "record the renewal: %v", err)
		}
		s.set(next, false, event)
		for _, sv := range live {
			if err := error(nil); err != nil {
				return Status{}, machine.NewError(machine.ErrFailed,
					"supervisor of %q did not take the renewal: %v", sv.Machine, err)
			}
		}
	}

	if err := s.stopKilled(ctx, machines, killed); err != nil {
		return Status{}, machine.NewError(machine.ErrFailed, "renewal applied; %v", err)
	}
	return status(next, false, s.clock.Now()), nil
}

// converge brings every live supervisor to the recorded expiry.
func (s *service) converge(ctx context.Context, expiry BootTime) error {
	live, err := s.sup.Live(ctx)
	if err != nil {
		return machine.NewError(machine.ErrFailed, "list the supervisors: %v", err)
	}
	for _, sv := range live {
		if sv.Expiry == expiry {
			continue
		}
		if err := s.sup.SetExpiry(ctx, sv.Machine, expiry); err != nil {
			return machine.NewError(machine.ErrFailed, "supervisor of %q did not take the expiry: %v", sv.Machine, err)
		}
	}
	return nil
}

// stopKilled brings the Machines whose supervisors were killed to Stopped.
func (s *service) stopKilled(ctx context.Context, machines Machines, ids []machine.MachineID) error {
	var errs []error
	for _, id := range ids {
		if err := machines.ForceStop(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("stop %q: %w", id, err))
		}
	}
	return errors.Join(errs...)
}
