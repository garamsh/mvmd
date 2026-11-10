package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// pipelinePlan is the plan Start gives the pipeline, over the env's own
// adapters, with a guest step that succeeds.
func (e *env) pipelinePlan(id machine.MachineID) machine.BootPlan {
	return machine.BootPlan{
		Describe: func(ctx context.Context, boot machine.BootSpec) (machine.ProcessDescription, error) {
			return e.driver.Describe(ctx, id, boot)
		},
		VMM:   func(ctx context.Context, boot machine.BootSpec) error { return e.driver.Boot(ctx, id, boot) },
		Guest: func(context.Context) error { return nil },
	}
}

// pipelineEnv is an env with a Created Machine "m", which is the Machine the
// pipeline brings up.
func pipelineEnv(t *testing.T) (*env, machine.Machine) {
	t.Helper()
	e := newEnv()
	e.created(t, "m")
	m, err := e.svc.Inspect(context.Background(), "m")
	require.NoError(t, err)
	return e, m
}

// requireUnwound requires that nothing the pipeline began is left: no VMM
// process and no tap.
func requireUnwound(t *testing.T, e *env, id machine.MachineID) {
	t.Helper()
	_, running := e.sup.Running(id)
	assert.False(t, running, "the VMM was ended")
	_, applied := e.net.Applied(id)
	assert.False(t, applied, "the tap was removed")
}

// boundedService is a service over the env's adapters that waits at most d for
// a VM or a guest.
func boundedService(e *env, d time.Duration) machine.Service {
	return machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithBootTimeout(d))
}

// The pipeline reports the step that failed and what it did about it: a
// failure at any step leaves no VMM and no tap, and says whether a VMM existed.
func TestBringUp_AFailedStepEndsTheVMMAndWithdrawsTheNetwork(t *testing.T) {
	for _, tt := range []struct {
		name   string
		fail   func(e *env, p *machine.BootPlan)
		kind   error
		reason string
		vmm    bool
	}{
		{"the run directory cannot be named", func(e *env, _ *machine.BootPlan) { e.sup.FailAt("run-dir", errBoom) }, machine.ErrFailed, "name the run directory", false},
		{"the network cannot be applied", func(e *env, _ *machine.BootPlan) { e.net.FailAt("apply", errBoom) }, machine.ErrPreconditionFailed, "the network policy was not applied", false},
		{"the process cannot be described", func(e *env, _ *machine.BootPlan) { e.driver.FailAt("describe", errBoom) }, machine.ErrFailed, "describe the VMM process", false},
		{"the supervisor cannot start", func(e *env, _ *machine.BootPlan) { e.sup.FailAt("start", errBoom) }, machine.ErrFailed, "start the VMM process", false},
		{"the VMM step fails", func(e *env, _ *machine.BootPlan) { e.driver.FailAt("boot", errBoom) }, machine.ErrFailed, errBoom.Error(), true},
		{"the guest step fails", func(_ *env, p *machine.BootPlan) {
			p.Guest = func(context.Context) error { return errBoom }
		}, machine.ErrFailed, errBoom.Error(), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e, m := pipelineEnv(t)
			plan := e.pipelinePlan("m")
			tt.fail(e, &plan)

			out := machine.BringUp(context.Background(), e.svc, m, plan)
			require.NotNil(t, out.Failure)
			assert.ErrorIs(t, out.Failure, tt.kind)
			assert.Contains(t, out.Reason, tt.reason)
			assert.Equal(t, tt.vmm, out.VMM, "whether a VMM process existed")
			assert.NoError(t, out.EndErr)
			assert.NoError(t, out.WithdrawErr)
			requireUnwound(t, e, "m")
		})
	}

	t.Run("control: with every step working the VMM runs and holds its tap", func(t *testing.T) {
		e, m := pipelineEnv(t)

		out := machine.BringUp(context.Background(), e.svc, m, e.pipelinePlan("m"))
		require.Nil(t, out.Failure)
		_, running := e.sup.Running("m")
		assert.True(t, running)
		_, applied := e.net.Applied("m")
		assert.True(t, applied)
	})
}

// A VMM that ends before its VM runs is found without waiting for the bound.
func TestBringUp_AVMMThatEndedBeforeItsVMRunsIsReported(t *testing.T) {
	e, m := pipelineEnv(t)
	svc := e.waitEnv(exitsAtStart{e.sup})
	plan := e.pipelinePlan("m")
	plan.VMM = func(ctx context.Context, _ machine.BootSpec) error {
		<-ctx.Done()
		return ctx.Err()
	}

	var out machine.BootOutcome
	requireAnswer(t, func() error { out = machine.BringUp(context.Background(), svc, m, plan); return nil })
	require.NotNil(t, out.Failure)
	assert.Contains(t, out.Reason, "the VMM ended before its VM reached Running")
	assert.True(t, out.VMM)
	requireUnwound(t, e, "m")
}

// A VMM that cannot be ended is reported, and it keeps the network it holds,
// since the tap of a running VMM is not taken from it.
func TestBringUp_AVMMThatCannotBeEndedKeepsItsNetwork(t *testing.T) {
	e, m := pipelineEnv(t)
	plan := e.pipelinePlan("m")
	plan.Guest = func(context.Context) error { return errBoom }
	e.sup.FailAt("terminate", errBoom)

	out := machine.BringUp(context.Background(), e.svc, m, plan)
	require.NotNil(t, out.Failure)
	require.Error(t, out.EndErr)
	assert.Contains(t, out.Reason, "the VMM could not be ended")
	assert.True(t, out.VMM)
	_, running := e.sup.Running("m")
	assert.True(t, running)
	_, applied := e.net.Applied("m")
	assert.True(t, applied, "the tap stays")
}

// A network that cannot be removed is reported after the VMM was ended.
func TestBringUp_ANetworkThatCannotBeRemovedIsReported(t *testing.T) {
	e, m := pipelineEnv(t)
	plan := e.pipelinePlan("m")
	plan.Guest = func(context.Context) error { return errBoom }
	e.net.FailAt("withdraw", errBoom)

	out := machine.BringUp(context.Background(), e.svc, m, plan)
	require.NotNil(t, out.Failure)
	require.Error(t, out.WithdrawErr)
	assert.NoError(t, out.EndErr)
	assert.Contains(t, out.Reason, "the network could not be removed")
	_, running := e.sup.Running("m")
	assert.False(t, running, "the VMM was ended first")
}

// A guest step that does not return within the boot timeout fails the
// pipeline, whatever the step does with its context.
func TestBringUp_AGuestStepThatNeverReturnsFailsAfterTheBootTimeout(t *testing.T) {
	e, m := pipelineEnv(t)
	svc := boundedService(e, bootBound)
	release := make(chan struct{})
	defer close(release)
	plan := e.pipelinePlan("m")
	plan.Guest = func(context.Context) error { <-release; return nil }

	begun := time.Now()
	var out machine.BootOutcome
	requireAnswer(t, func() error { out = machine.BringUp(context.Background(), svc, m, plan); return nil })
	require.NotNil(t, out.Failure)
	assert.GreaterOrEqual(t, time.Since(begun), bootBound, "not before the bound")
	assert.Contains(t, out.Reason, "the guest did not finish booting within "+bootBound.String())
	requireUnwound(t, e, "m")
}

// A guest that does not finish catching up does not keep the restore for ever:
// when the boot timeout passes the VMM is ended and no Machine exists, though
// the Guest adapter ignores the context it is given.
func TestRestoreSnapshot_AGuestThatNeverCatchesUpFailsAfterTheBootTimeout(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	svc := boundedService(e, bootBound)
	entered, release := e.guest.Hold("catch-up")
	defer release()

	begun := time.Now()
	done := make(chan error, 1)
	go func() { done <- svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy) }()
	requireHeld(t, entered)

	err := requireDone(t, done)
	requireKind(t, err, machine.ErrFailed)
	assert.GreaterOrEqual(t, time.Since(begun), bootBound, "not before the bound")
	assert.Contains(t, err.Error(), "the guest did not finish catching up within "+bootBound.String())
	_, inspected := svc.Inspect(ctx, "new")
	requireKind(t, inspected, machine.ErrNotFound)
	_, running := e.sup.Running("new")
	assert.False(t, running, "the VMM was ended")
	assert.False(t, e.net.Allocated("new"), "the network is released")
	_, held := e.disks.Holds("new-root")
	assert.False(t, held, "the root disk is removed")

	// Control: a catch-up that finishes within the bound restores.
	other := boundedService(e, 5*time.Second)
	require.NoError(t, other.RestoreSnapshot(ctx, "again", "r-again", "snap", narrowPolicy))
	_, running = e.sup.Running("again")
	assert.True(t, running)
}
