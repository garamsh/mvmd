package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// exitsAtStart is a Supervision whose VMM ends right after the supervisor has
// reported its start: the supervisor is gone by the time anything asks for it.
type exitsAtStart struct{ machine.Supervision }

func (s exitsAtStart) Start(ctx context.Context, id machine.MachineID, process machine.ProcessDescription, user machine.User) error {
	if err := s.Supervision.Start(ctx, id, process, user); err != nil {
		return err
	}
	return s.Supervision.Terminate(ctx, id)
}

// neverRuns is a Driver whose VM does not reach Running, as a Driver waiting
// for an API socket the VMM never bound: Boot and Restore honour the context
// they are given and return only when it ends.
type neverRuns struct{ machine.Driver }

func (neverRuns) Boot(ctx context.Context, _ machine.MachineID, _ machine.BootSpec) error {
	<-ctx.Done()
	return ctx.Err()
}

func (neverRuns) Restore(ctx context.Context, _ machine.MachineID, _ machine.BootSpec) error {
	<-ctx.Done()
	return ctx.Err()
}

// waitEnv is an env whose service runs on a Driver that never reaches Running,
// over a Supervision that is the given one, with every bound but the one under
// test far above what a test waits. The control is the same service over the
// env's own Driver, which is the success the other tests of Start and
// RestoreSnapshot already show.
func (e *env) waitEnv(sup machine.Supervision, options ...machine.Option) machine.Service {
	options = append([]machine.Option{machine.WithCallTimeout(time.Minute), machine.WithBootTimeout(time.Minute), machine.WithVMMEndPoll(10 * time.Millisecond)}, options...)
	return machine.NewService(neverRuns{e.driver}, sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, options...)
}

// requireAnswer returns the call's result, failing with the time it took when
// it does not answer within a few boot bounds.
func requireAnswer(t *testing.T, call func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- call() }()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the call did not answer within 5s, though every bound it has is shorter or the VMM had ended")
		return nil
	}
}

// A VMM that ends once its supervisor has reported the start leaves nothing for
// the Driver's wait to see, so the wait is cut by the supervisor's absence:
// the restore fails, undone, with no Machine.
func TestRestoreSnapshot_AVMMThatEndsAtStartFailsAtOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	svc := e.waitEnv(exitsAtStart{e.sup})

	err := requireAnswer(t, func() error { return svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy) })
	requireKind(t, err, machine.ErrFailed)
	assert.Contains(t, err.Error(), "the VMM ended")
	_, inspected := svc.Inspect(ctx, "new")
	requireKind(t, inspected, machine.ErrNotFound)
	assert.False(t, e.net.Allocated("new"), "the network is released")
	_, running := e.sup.Running("new")
	assert.False(t, running)
	_, held := e.disks.Holds("new-root")
	assert.False(t, held, "the root disk is removed")
}

// A VM that does not reach Running while its VMM lives is bounded by the boot
// timeout, and not by the call's.
func TestRestoreSnapshot_AVMThatNeverRunsFailsAfterTheBootTimeout(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	svc := e.waitEnv(e.sup, machine.WithBootTimeout(bootBound))

	begun := time.Now()
	err := requireAnswer(t, func() error { return svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy) })
	requireKind(t, err, machine.ErrFailed)
	assert.GreaterOrEqual(t, time.Since(begun), bootBound, "not before the bound")
	assert.Contains(t, err.Error(), "the VMM did not reach Running within "+bootBound.String())
	_, inspected := svc.Inspect(ctx, "new")
	requireKind(t, inspected, machine.ErrNotFound)
	_, running := e.sup.Running("new")
	assert.False(t, running, "the VMM was ended")
}

// Start waits on the same Driver step, so it is bounded the same ways: the
// Machine is Stopped with the reason, its VMM ended and its tap removed.
func TestStart_AVMMThatEndsAtStartStopsTheMachineAtOnce(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	svc := e.waitEnv(exitsAtStart{e.sup})
	require.NoError(t, svc.Create(ctx, "m", "req-m", baseSpec()))

	err := requireAnswer(t, func() error { return svc.Start(ctx, "m") })
	requireKind(t, err, machine.ErrFailed)
	m, inspected := svc.Inspect(ctx, "m")
	require.NoError(t, inspected)
	assert.Equal(t, machine.StateStopped, m.State)
	assert.Contains(t, m.FailureReason, "the VMM ended")
	_, applied := e.net.Applied("m")
	assert.False(t, applied, "the tap was removed")
}

func TestStart_AVMThatNeverRunsStopsTheMachineAfterTheBootTimeout(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	svc := e.waitEnv(e.sup, machine.WithBootTimeout(bootBound))
	require.NoError(t, svc.Create(ctx, "m", "req-m", baseSpec()))

	begun := time.Now()
	err := requireAnswer(t, func() error { return svc.Start(ctx, "m") })
	requireKind(t, err, machine.ErrFailed)
	assert.GreaterOrEqual(t, time.Since(begun), bootBound, "not before the bound")
	m, inspected := svc.Inspect(ctx, "m")
	require.NoError(t, inspected)
	assert.Equal(t, machine.StateStopped, m.State)
	assert.Contains(t, m.FailureReason, "the VMM did not reach Running within "+bootBound.String())
	_, running := e.sup.Running("m")
	assert.False(t, running, "the VMM was ended")
}
