package fencing

import (
	"context"
	"errors"
	"fmt"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func (s *service) Reconcile(ctx context.Context, machines Machines) error {
	s.order.Lock()
	rec, ok, err := s.store.Load(ctx)
	if err != nil {
		s.order.Unlock()
		return fmt.Errorf("load the deadline record: %w", err)
	}
	s.mu.Lock()
	s.machines = machines
	s.mu.Unlock()

	switch {
	case !ok, rec.BootID != s.clock.BootID():
		// No record, or a reboot: no supervisor or VM survived it, and the
		// deadline is not armed until a renewal arms it.
		s.set(nil, false)
		s.order.Unlock()
		return nil
	case s.clock.Now() >= rec.Expiry:
		// A passed expiry is never extended: everything stops first.
		s.set(&rec, false)
		s.order.Unlock()
		return s.expire(ctx)
	}

	// Otherwise the recorded expiry is passed to each supervisor that
	// survived, and the time left is what is left of it, never the length. A
	// supervisor that cannot be reached still enforces the earlier expiry it
	// holds (ADR 0003), so it is not an error here.
	s.set(&rec, false)
	live, err := s.sup.Live(ctx)
	if err != nil {
		s.order.Unlock()
		return fmt.Errorf("list the supervisors: %w", err)
	}
	for _, sv := range live {
		_ = s.sup.SetExpiry(ctx, sv.Machine, rec.Expiry)
	}
	s.order.Unlock()
	return nil
}

func (s *service) Run(ctx context.Context) error {
	for {
		s.mu.Lock()
		rec, stopped, changed := s.rec, s.stopped, s.changed
		s.mu.Unlock()

		if rec == nil || stopped {
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return ctx.Err()
			}
		}

		// Wait for the expiry, or for a renewal that moves it.
		wait, cancel := context.WithCancel(ctx)
		go func() {
			select {
			case <-changed:
				cancel()
			case <-wait.Done():
			}
		}()
		err := s.clock.Wait(wait, rec.Expiry)
		cancel()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			continue
		}
		if err := s.expire(ctx); err != nil {
			return err
		}
	}
}

// expire stops every Machine by force if the expiry has passed: no grace
// period, supervisors killed through their port, disks kept. Nothing starts
// again until a later renewal.
func (s *service) expire(ctx context.Context) error {
	s.order.Lock()
	rec, stopped, machines := s.current()
	if rec == nil || stopped || s.clock.Now() < rec.Expiry {
		s.order.Unlock()
		return nil
	}
	live, err := s.sup.Live(ctx)
	if err != nil {
		s.order.Unlock()
		return fmt.Errorf("list the supervisors: %w", err)
	}
	var errs []error
	for _, sv := range live {
		if err := s.sup.Terminate(ctx, sv.Machine); err != nil {
			errs = append(errs, fmt.Errorf("kill the supervisor of %q: %w", sv.Machine, err))
		}
	}
	if len(errs) > 0 {
		s.order.Unlock()
		return errors.Join(errs...)
	}
	s.set(rec, true)
	s.order.Unlock()

	// The supervisors are gone; the lifecycle learns that its Machines stopped.
	all, err := machines.List(ctx)
	if err != nil {
		return fmt.Errorf("list the Machines: %w", err)
	}
	for _, m := range all {
		if m.State == machine.StateRunning || m.State == machine.StatePaused || m.State == machine.StateFaulted {
			if err := machines.ForceStop(ctx, m.ID); err != nil {
				errs = append(errs, fmt.Errorf("stop %q: %w", m.ID, err))
			}
		}
	}
	return errors.Join(errs...)
}
