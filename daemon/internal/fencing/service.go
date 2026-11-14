package fencing

import (
	"context"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Service is the node's fencing deadline. It is also the lifecycle's start
// permission and its supervision: Start hands a supervisor the current expiry
// in the same step that checks the deadline is armed, so no supervisor exists
// without one (ADR 0003).
type Service interface {
	// Renew applies a renewal: a number greater than the last applied sets the
	// expiry to the length from now. Any other number changes nothing. It
	// returns once the record is durable and every live supervisor holds the
	// expiry, and reports the deadline as then held.
	Renew(ctx context.Context, length time.Duration, number uint64) (Status, error)
	// Inspect reports the deadline.
	Inspect(ctx context.Context) (Status, error)
	// Reconcile runs once on start, before any call is served. It compares the
	// recorded expiry with the boot clock and brings the supervisors that
	// survived to it, or stops everything if it has passed. The Machines are
	// kept for the stops that follow.
	Reconcile(ctx context.Context, machines Machines) error
	// Run stops every Machine by force when the expiry passes, and returns when
	// the context ends or a stop fails.
	Run(ctx context.Context) error

	// RenewFence and InspectFence are Renew and Inspect for the own fence of
	// a Machine created with one (ADR 0064).
	RenewFence(ctx context.Context, id machine.MachineID, length time.Duration, number uint64) (Status, error)
	InspectFence(ctx context.Context, id machine.MachineID) (Status, error)

	machine.StartPermitter
	machine.Supervision
	machine.Fences
}

type service struct {
	clock Clock
	store DeadlineStore
	sup   Supervisors
	// events hears of each change of the deadline, from set.
	events Events

	// order serializes starts, renewals and the stop at expiry against one
	// another: the only node-wide ordering ADR 0003 names. Nothing under it
	// waits on a Machine's own call, so a long stop never delays a renewal.
	order sync.Mutex

	// mu guards the fields below, and is never held across a port call.
	mu       sync.Mutex
	rec      *Record
	stopped  bool
	machines Machines
	changed  chan struct{}
	// fences holds each fenced Machine's own fence; nil until loaded.
	fences map[machine.MachineID]Record

	fenceStore FenceStore
	// fenceLoad serializes the one load of the fence records.
	fenceLoad sync.Mutex
}

// NewService returns a deadline that is unarmed until Reconcile and a renewal.
func NewService(clock Clock, store DeadlineStore, sup Supervisors, events Events, options ...Option) Service {
	s := &service{clock: clock, store: store, sup: sup, events: events, changed: make(chan struct{})}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *service) current() (rec *Record, stopped bool, machines Machines) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, s.stopped, s.machines
}

// set replaces the deadline, the one place it changes, and wakes Run. A
// non-zero event is reported to Events. Every caller holds the ordering lock,
// so events come in the order the changes were made.
func (s *service) set(rec *Record, stopped bool, event Event) {
	s.mu.Lock()
	s.rec, s.stopped = rec, stopped
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
	if event != 0 {
		s.events.Deadline(event)
	}
}

func status(rec *Record, stopped bool, now BootTime) Status {
	switch {
	case rec == nil:
		return Status{}
	case now < rec.Expiry:
		return Status{Phase: PhaseArmed, Length: rec.Length, Remaining: rec.Expiry.Sub(now), Renewal: rec.Renewal}
	case stopped:
		return Status{Phase: PhaseStopped, Length: rec.Length, Renewal: rec.Renewal}
	default:
		return Status{Phase: PhaseExpired, Length: rec.Length, Renewal: rec.Renewal}
	}
}

func (s *service) Inspect(ctx context.Context) (Status, error) {
	rec, stopped, _ := s.current()
	return status(rec, stopped, s.clock.Now()), nil
}

func (s *service) PermitStart(ctx context.Context, id machine.MachineID) error {
	rec, stopped, machines := s.current()
	if err := permitted(machines, status(rec, stopped, s.clock.Now())); err != nil {
		return err
	}
	return s.permitFence(ctx, id)
}

func permitted(machines Machines, st Status) error {
	switch {
	case machines == nil:
		return machine.NewError(machine.ErrPreconditionFailed, "the fencing deadline has not been reconciled")
	case st.Phase != PhaseArmed:
		return machine.NewError(machine.ErrPreconditionFailed, "the fencing deadline is %s", st.Phase)
	}
	return nil
}

// RunDir returns the Machine's run directory as the supervisors name it.
func (s *service) RunDir(id machine.MachineID) (string, error) {
	return s.sup.RunDir(id)
}

// Start starts the Machine's supervisor with the current expiry. The check
// and the hand-over are one step under the ordering lock, so a renewal never
// falls between them.
func (s *service) Start(ctx context.Context, id machine.MachineID, process machine.ProcessDescription, user machine.User) error {
	s.order.Lock()
	defer s.order.Unlock()
	rec, stopped, machines := s.current()
	if err := permitted(machines, status(rec, stopped, s.clock.Now())); err != nil {
		return err
	}
	if err := s.permitFence(ctx, id); err != nil {
		return err
	}
	return s.sup.Start(ctx, id, process, user, s.effective(id, rec.Expiry))
}

// Live lists the Machines whose supervisors exist: the same listing the
// deadline walks, so the lifecycle checks its recorded Machines against it.
func (s *service) Live(ctx context.Context) ([]machine.MachineID, error) {
	live, err := s.sup.Live(ctx)
	if err != nil {
		return nil, err
	}
	ids := make([]machine.MachineID, len(live))
	for i, sv := range live {
		ids[i] = sv.Machine
	}
	return ids, nil
}

// RemoveState removes the Machine's state directory. Like Terminate it holds
// no expiry, so it is not ordered against renewals.
func (s *service) RemoveState(ctx context.Context, id machine.MachineID) error {
	return s.sup.RemoveState(ctx, id)
}

// ListState lists the Machines that have a state directory.
func (s *service) ListState(ctx context.Context) ([]machine.MachineID, error) {
	return s.sup.ListState(ctx)
}

// Terminate forces the Machine's supervisor and VMM down. It is not ordered
// against renewals: a supervisor that is gone holds no expiry.
func (s *service) Terminate(ctx context.Context, id machine.MachineID) error {
	return s.sup.Terminate(ctx, id)
}
