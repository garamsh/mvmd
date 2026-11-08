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

	machine.StartPermitter
	machine.Supervision
}

type service struct {
	clock Clock
	store DeadlineStore
	sup   Supervisors

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
}

// NewService returns a deadline that is unarmed until Reconcile and a renewal.
func NewService(clock Clock, store DeadlineStore, sup Supervisors) Service {
	return &service{clock: clock, store: store, sup: sup, changed: make(chan struct{})}
}

func (s *service) current() (rec *Record, stopped bool, machines Machines) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rec, s.stopped, s.machines
}

// set replaces the deadline and wakes Run.
func (s *service) set(rec *Record, stopped bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rec, s.stopped = rec, stopped
	close(s.changed)
	s.changed = make(chan struct{})
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
	return permitted(machines, status(rec, stopped, s.clock.Now()))
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
	return s.sup.Start(ctx, id, process, user, rec.Expiry)
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

// Terminate forces the Machine's supervisor and VMM down. It is not ordered
// against renewals: a supervisor that is gone holds no expiry.
func (s *service) Terminate(ctx context.Context, id machine.MachineID) error {
	return s.sup.Terminate(ctx, id)
}
