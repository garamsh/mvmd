package machine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"
)

// bootPlan is what a call that brings a VMM up chooses; the sequence is the
// pipeline's. Start and RestoreSnapshot each fill one in.
type bootPlan struct {
	machine Machine
	// describe is the Driver's description of the VMM process.
	describe func(ctx context.Context, boot BootSpec) (ProcessDescription, error)
	// vmm is the Driver step that returns once the started VMM's VM runs.
	vmm func(ctx context.Context, boot BootSpec) error
	// guest is the step that tells the guest what it needs, given the image as
	// the node holds it. Its errors are framed by the plan.
	guest func(ctx context.Context, image LocatedImage) error
	// guestWhile names what the guest step is doing, for the reason given
	// when it outlasts the boot timeout.
	guestWhile string
}

// begun is what a bring-up has made that its unwind removes.
type begun struct{ network, vmm bool }

// bootResult is the outcome of a bring-up or of its unwind. failure is nil when
// the VMM is up. Otherwise the first-stage unwind has run: endErr is set when
// the VMM that exists could not be ended, in which case its network is kept,
// and withdrawErr when the network could not be removed.
type bootResult struct {
	failure     *Error
	begun       begun
	endErr      error
	withdrawErr error
}

// reason is the failure's reason, extended by what the unwind could not do.
func (r bootResult) reason() string {
	reason := r.failure.Reason
	if r.endErr != nil {
		reason += fmt.Sprintf("; the VMM could not be ended: %v", r.endErr)
	}
	if r.withdrawErr != nil {
		reason += fmt.Sprintf("; the network could not be removed: %v", r.withdrawErr)
	}
	return reason
}

// bringUp runs the sequence that makes a Machine's VMM run: the run directory,
// the block devices, the network, the Driver's description, the VMM under its
// supervisor (the step that hands over the current fencing expiry), the wait
// for its VM, and the guest step. A failure ends the VMM it started and
// withdraws the network it applied, and says what could not be undone.
func (s *service) bringUp(ctx context.Context, p bootPlan) bootResult {
	m := p.machine
	id := m.ID
	var up begun
	stopped := func(failure *Error) bootResult { return s.unwind(ctx, id, up, failure) }

	boot := BootSpec{Resources: m.Resources, Ceiling: m.Ceiling}
	var err error
	if boot.RunDir, err = s.supervisor.RunDir(id); err != nil {
		return stopped(fail(ErrFailed, "name the run directory: %v", err))
	}
	devices, image, err := s.bootDevices(ctx, m)
	if err != nil {
		var e *Error
		if !errors.As(err, &e) {
			e = fail(ErrFailed, "%v", err)
		}
		return stopped(e)
	}
	boot.Devices = devices
	tap, err := s.network.Apply(ctx, id, m.User, m.Policy)
	if err != nil {
		return stopped(fail(ErrPreconditionFailed, "the network policy was not applied: %v", err))
	}
	up.network = true
	boot.Network = NetworkDevice{Tap: tap, MAC: m.Network.MAC}
	process, err := p.describe(ctx, boot)
	if err != nil {
		return stopped(fail(ErrFailed, "describe the VMM process: %v", err))
	}
	if err := s.supervisor.Start(ctx, id, process.For(boot), m.User); err != nil {
		return stopped(fail(ErrFailed, "start the VMM process: %v", err))
	}
	up.vmm = true
	if err := s.awaitVMM(ctx, id, func(ctx context.Context) error { return p.vmm(ctx, boot) }); err != nil {
		return stopped(fail(ErrFailed, "%v", err))
	}
	if err := s.awaitGuest(ctx, p.guestWhile, func(ctx context.Context) error { return p.guest(ctx, image) }); err != nil {
		return stopped(fail(ErrFailed, "%v", err))
	}
	return bootResult{begun: up}
}

// unwind ends what a bring-up made, as far as up says it got, and returns the
// failure with what could not be undone. A caller whose own step after a
// successful bring-up fails uses it too.
func (s *service) unwind(ctx context.Context, id MachineID, up begun, failure *Error) bootResult {
	res := bootResult{failure: failure, begun: up}
	if !up.network {
		return res
	}
	ctx, cancel := s.compensating(ctx)
	defer cancel()
	if up.vmm {
		if err := s.supervisor.Terminate(ctx, id); err != nil {
			res.endErr = err
			return res
		}
	}
	res.withdrawErr = s.network.Withdraw(ctx, id)
	return res
}

// awaitGuest runs the guest step for at most the node's boot timeout. The bound
// is the lifecycle's own: it holds when the caller's context never ends and
// when the Guest adapter ignores the context it is given, and a caller that
// gives up does not cancel the step, since an accepted call completes or fails
// into its defined state.
func (s *service) awaitGuest(ctx context.Context, while string, step func(context.Context) error) error {
	guestCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- step(guestCtx) }()
	timer := time.NewTimer(s.bootTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("the guest did not finish %s within %s", while, s.bootTimeout)
	}
}

// awaitVMM runs a Driver step that waits for the started VMM's VM to run, for
// at most the node's boot timeout, and gives up when the Machine's supervisor
// is no longer listed, which is the VMM having ended (ADR 0003). The Driver
// waits on a socket the VMM binds, which a VMM that has ended never does, so the
// step's own end would come only with the call's bound. Like awaitGuest it holds
// when the Driver ignores the context it is given.
func (s *service) awaitVMM(ctx context.Context, id MachineID, step func(context.Context) error) error {
	stepCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- step(stepCtx) }()
	timer := time.NewTimer(s.bootTimeout)
	defer timer.Stop()
	tick := time.NewTicker(s.vmmEndPoll)
	defer tick.Stop()
	for {
		select {
		case err := <-done:
			return err
		case <-timer.C:
			return fmt.Errorf("the VMM did not reach Running within %s", s.bootTimeout)
		case <-tick.C:
			// A listing that fails observes nothing; the next one tries again.
			if live, err := s.supervisor.Live(ctx); err == nil && !slices.Contains(live, id) {
				return errors.New("the VMM ended before its VM reached Running")
			}
		}
	}
}
