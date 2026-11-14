package fencing

import (
	"context"
	"fmt"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Option configures a Service.
type Option func(*service)

// WithFences gives the Service the store of the Machines' own fences (ADR
// 0064). Without it no Machine can be created with one.
func WithFences(store FenceStore) Option {
	return func(s *service) { s.fenceStore = store }
}

// loadFences reads the records once, the first time any call needs them: the
// lifecycle asks about a lapse in Restore, before Reconcile runs. A record
// of another boot is kept as an unarmed one, as the node's deadline is after a
// reboot: no supervisor or VM survived it.
func (s *service) loadFences(ctx context.Context) error {
	s.fenceLoad.Lock()
	defer s.fenceLoad.Unlock()
	s.mu.Lock()
	loaded := s.fences != nil
	s.mu.Unlock()
	if loaded {
		return nil
	}
	fences := map[machine.MachineID]Record{}
	if s.fenceStore != nil {
		stored, err := s.fenceStore.Load(ctx)
		if err != nil {
			return fmt.Errorf("load the fence records: %w", err)
		}
		for id, rec := range stored {
			if rec.BootID != s.clock.BootID() {
				rec = Record{}
			}
			fences[id] = rec
		}
	}
	s.mu.Lock()
	s.fences = fences
	s.mu.Unlock()
	return nil
}

// fence returns the Machine's own fence, and whether it has one.
func (s *service) fence(ctx context.Context, id machine.MachineID) (Record, bool, error) {
	if err := s.loadFences(ctx); err != nil {
		return Record{}, false, machine.NewError(machine.ErrFailed, "%v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.fences[id]
	return rec, ok, nil
}

func (s *service) setFence(id machine.MachineID, rec Record) {
	s.mu.Lock()
	s.fences[id] = rec
	s.mu.Unlock()
}

// fenceArmed says whether the record is an expiry of this boot that has not
// passed.
func (s *service) fenceArmed(rec Record, now BootTime) bool {
	return rec.Renewal > 0 && rec.BootID == s.clock.BootID() && now < rec.Expiry
}

// fenceStatus is the Machine's fence as Inspect shows it. It is never
// stopped-everything: a lapse ends one VMM, and the Machine is Stopped.
func (s *service) fenceStatus(rec Record, now BootTime) Status {
	switch {
	case rec.Renewal == 0 || rec.BootID != s.clock.BootID():
		return Status{}
	case now < rec.Expiry:
		return Status{Phase: PhaseArmed, Length: rec.Length, Remaining: rec.Expiry.Sub(now), Renewal: rec.Renewal}
	default:
		return Status{Phase: PhaseExpired, Length: rec.Length, Renewal: rec.Renewal}
	}
}

// effective is the expiry the Machine's supervisor holds: the earlier of the
// node's and the Machine's own, so that either one ends the VMM (ADR 0064).
func (s *service) effective(id machine.MachineID, node BootTime) BootTime {
	s.mu.Lock()
	rec, ok := s.fences[id]
	s.mu.Unlock()
	if ok && rec.Renewal > 0 && rec.BootID == s.clock.BootID() && rec.Expiry < node {
		return rec.Expiry
	}
	return node
}

// Enrol implements machine.Fences.
func (s *service) Enrol(ctx context.Context, id machine.MachineID) error {
	if s.fenceStore == nil {
		return machine.NewError(machine.ErrUnsupported, "the node keeps no fences of Machines")
	}
	if err := s.loadFences(ctx); err != nil {
		return machine.NewError(machine.ErrFailed, "%v", err)
	}
	s.order.Lock()
	defer s.order.Unlock()
	// A new Machine starts unarmed, whatever an earlier one of the name left.
	if err := s.fenceStore.Save(ctx, id, Record{}); err != nil {
		return machine.NewError(machine.ErrFailed, "record the Machine's fence: %v", err)
	}
	s.setFence(id, Record{})
	return nil
}

// Release implements machine.Fences.
func (s *service) Release(ctx context.Context, id machine.MachineID) error {
	if s.fenceStore == nil {
		return nil
	}
	if err := s.loadFences(ctx); err != nil {
		return machine.NewError(machine.ErrFailed, "%v", err)
	}
	s.order.Lock()
	defer s.order.Unlock()
	if err := s.fenceStore.Delete(ctx, id); err != nil {
		return machine.NewError(machine.ErrFailed, "remove the Machine's fence: %v", err)
	}
	s.mu.Lock()
	delete(s.fences, id)
	s.mu.Unlock()
	return nil
}

// Lapse implements machine.Fences. A VMM that ended after the Machine's own
// expiry, with the node's deadline not earlier, ended by its fence: the
// supervisor counts that expiry (ADR 0003), and mvmd compares it on the same
// boot clock. It reports how long ago the fence lapsed.
func (s *service) Lapse(ctx context.Context, id machine.MachineID) (time.Duration, bool, error) {
	rec, ok, err := s.fence(ctx, id)
	if err != nil || !ok || rec.Renewal == 0 || rec.BootID != s.clock.BootID() {
		return 0, false, err
	}
	now := s.clock.Now()
	if now < rec.Expiry {
		return 0, false, nil
	}
	// Read from the store: the lifecycle asks in Restore, before Reconcile.
	node, found, err := s.store.Load(ctx)
	if err != nil {
		return 0, false, machine.NewError(machine.ErrFailed, "load the deadline record: %v", err)
	}
	if found && node.BootID == s.clock.BootID() && node.Expiry < rec.Expiry {
		// The node's deadline passed first, and its stop is the end.
		return 0, false, nil
	}
	return now.Sub(rec.Expiry), true, nil
}

// permitFence refuses a start of a Machine whose own fence is not armed.
func (s *service) permitFence(ctx context.Context, id machine.MachineID) error {
	rec, ok, err := s.fence(ctx, id)
	if err != nil || !ok {
		return err
	}
	if st := s.fenceStatus(rec, s.clock.Now()); st.Phase != PhaseArmed {
		return machine.NewError(machine.ErrPreconditionFailed, "the Machine's own fence is %s", st.Phase)
	}
	return nil
}

// InspectFence reports the Machine's own fence.
func (s *service) InspectFence(ctx context.Context, id machine.MachineID) (Status, error) {
	rec, ok, err := s.fence(ctx, id)
	if err != nil {
		return Status{}, err
	}
	if !ok {
		return Status{}, machine.NewError(machine.ErrPreconditionFailed, "Machine %q has no fence of its own", id)
	}
	return s.fenceStatus(rec, s.clock.Now()), nil
}

// RenewFence applies a renewal to the Machine's own fence, as Renew does to the
// node's: a number greater than the last applied sets the expiry to the length
// from now, any other changes nothing, and it returns once the record is
// durable and the Machine's supervisor, if any, holds the expiry. It starts
// nothing, and it moves no other Machine's expiry.
func (s *service) RenewFence(ctx context.Context, id machine.MachineID, length time.Duration, number uint64) (Status, error) {
	if length <= 0 {
		return Status{}, machine.NewError(machine.ErrInvalid, "the fence length %s is not positive", length)
	}
	if err := s.loadFences(ctx); err != nil {
		return Status{}, machine.NewError(machine.ErrFailed, "%v", err)
	}
	s.order.Lock()
	defer s.order.Unlock()
	rec, ok, _ := s.fence(ctx, id)
	if !ok {
		return Status{}, machine.NewError(machine.ErrPreconditionFailed, "Machine %q has no fence of its own", id)
	}
	node, _, machines := s.current()
	if machines == nil {
		return Status{}, machine.NewError(machine.ErrPreconditionFailed, "the fencing deadline has not been reconciled")
	}
	now := s.clock.Now()

	if false && number <= rec.Renewal && rec.BootID == s.clock.BootID() {
		if s.fenceArmed(rec, now) {
			if err := s.convergeOne(ctx, id, node, now); err != nil {
				return Status{}, err
			}
		}
		return s.fenceStatus(rec, now), nil
	}

	next := Record{Expiry: now.Add(length), BootID: s.clock.BootID(), Length: length, Renewal: number}
	killed := false
	if s.fenceArmed(rec, now) && next.Expiry < rec.Expiry {
		// As with the node's deadline, a shortening reaches the supervisor
		// before the record, and one that will not take it is killed.
		if err := s.pushFence(ctx, id, node, next, now); err != nil {
			if err := s.sup.Terminate(ctx, id); err != nil {
				return Status{}, machine.NewError(machine.ErrFailed,
					"supervisor of %q neither took the shorter expiry nor could be killed: %v", id, err)
			}
			killed = true
		}
		if err := s.fenceStore.Save(ctx, id, next); err != nil {
			return Status{}, machine.NewError(machine.ErrFailed, "record the renewal: %v", err)
		}
		s.setFence(id, next)
	} else {
		// An extension is recorded first and passed on second.
		if err := s.fenceStore.Save(ctx, id, next); err != nil {
			return Status{}, machine.NewError(machine.ErrFailed, "record the renewal: %v", err)
		}
		s.setFence(id, next)
		if err := s.pushFence(ctx, id, node, next, now); err != nil {
			return Status{}, machine.NewError(machine.ErrFailed, "supervisor of %q did not take the renewal: %v", id, err)
		}
	}
	if killed {
		if err := machines.ForceStop(ctx, id); err != nil {
			return Status{}, machine.NewError(machine.ErrFailed, "renewal applied; stop %q: %v", id, err)
		}
	}
	return s.fenceStatus(next, s.clock.Now()), nil
}

// pushFence gives the Machine's supervisor, when it has one and the node's
// deadline is armed, the earlier of the node's expiry and the renewed fence's.
func (s *service) pushFence(ctx context.Context, id machine.MachineID, node *Record, fence Record, now BootTime) error {
	if node == nil || now >= node.Expiry {
		return nil
	}
	live, err := s.sup.Live(ctx)
	if err != nil {
		return fmt.Errorf("list the supervisors: %w", err)
	}
	for _, sv := range live {
		if sv.Machine != id {
			continue
		}
		expiry := node.Expiry
		if fence.Expiry < expiry {
			expiry = fence.Expiry
		}
		return s.sup.SetExpiry(ctx, id, expiry)
	}
	return nil
}

// convergeOne brings the Machine's supervisor to the expiry it should hold.
func (s *service) convergeOne(ctx context.Context, id machine.MachineID, node *Record, now BootTime) error {
	if node == nil || now >= node.Expiry {
		return nil
	}
	live, err := s.sup.Live(ctx)
	if err != nil {
		return machine.NewError(machine.ErrFailed, "list the supervisors: %v", err)
	}
	want := s.effective(id, node.Expiry)
	for _, sv := range live {
		if sv.Machine == id && sv.Expiry != want {
			if err := s.sup.SetExpiry(ctx, id, want); err != nil {
				return machine.NewError(machine.ErrFailed, "supervisor of %q did not take the expiry: %v", id, err)
			}
		}
	}
	return nil
}
