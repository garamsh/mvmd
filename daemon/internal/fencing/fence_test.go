package fencing_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// fenceEnv is the deadline and the lifecycle over substitutes that outlive
// both, so that restart replaces the two as an mvmd restart does.
type fenceEnv struct {
	*env
	fences *memory.FenceStore
	states *machinememory.StateStore
	images *machinememory.ImageStore
	wall   *machinememory.WallClock
	// ended is how often the lifecycle looks for an end; a test runs Watch only
	// where it needs it.
	poll time.Duration
}

func newFenceEnv(t *testing.T) *fenceEnv {
	t.Helper()
	e := &fenceEnv{
		env:    &env{clock: memory.NewClock("boot-1"), store: memory.NewDeadlineStore(), events: memory.NewEvents()},
		fences: memory.NewFenceStore(),
		states: machinememory.NewStateStore(),
		images: machinememory.NewImageStore(),
		wall:   machinememory.NewWallClock(time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)),
		poll:   time.Millisecond,
	}
	e.sup = memory.NewSupervisors(e.store)
	e.driver = machinememory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: []machine.Capability{machine.CapabilitySnapshot}})
	e.disks = machinememory.NewDisk()
	e.snaps = machinememory.NewSnapshotStore()
	e.images.Provide(image)
	e.restart(t)
	return e
}

// restart makes a new deadline and a new lifecycle over the same stores, then
// restores the lifecycle and reconciles the deadline, in the order the daemon
// does.
func (e *fenceEnv) restart(t *testing.T) {
	t.Helper()
	e.svc = fencing.NewService(e.clock, e.store, e.sup, e.events, fencing.WithFences(e.fences))
	e.m = machine.NewService(e.driver, e.svc, machinememory.NewNetwork(), e.disks, e.images, e.snaps, machinememory.NewGuest(0), e.wall, e.states, e.svc,
		machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), machinememory.NewEvents(),
		machine.WithKernels(machinememory.Kernels()), machine.WithFences(e.svc), machine.WithBootIdentity(e.clock), machine.WithVMMEndPoll(e.poll))
	ctx := context.Background()
	require.NoError(t, e.m.Restore(ctx))
	require.NoError(t, e.svc.Reconcile(ctx, e.m))
}

// create makes a Machine, fenced or not.
func (e *fenceEnv) create(t *testing.T, id machine.MachineID, fenced, restartOnBoot bool) {
	t.Helper()
	s := spec()
	s.Fenced, s.RestartOnBoot = fenced, restartOnBoot
	require.NoError(t, e.m.Create(context.Background(), id, machine.RequestID("r-"+id), s))
}

func (e *fenceEnv) renewFence(t *testing.T, id machine.MachineID, length time.Duration, number uint64) fencing.Status {
	t.Helper()
	st, err := e.svc.RenewFence(context.Background(), id, length, number)
	require.NoError(t, err)
	return st
}

func (e *fenceEnv) inspect(t *testing.T, id machine.MachineID) machine.Machine {
	t.Helper()
	m, err := e.m.Inspect(context.Background(), id)
	require.NoError(t, err)
	return m
}

func (e *fenceEnv) expiry(t *testing.T, id machine.MachineID) fencing.BootTime {
	t.Helper()
	at, ok := e.sup.Expiry(id)
	require.True(t, ok, "supervisor of %q", id)
	return at
}

// watch runs the lifecycle's look for ended VMMs until the test ends.
func (e *fenceEnv) watch(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.m.Watch(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
}

// A Machine created with a fence of its own is refused Start while the fence is
// not armed, as the node's deadline refuses today; a Machine without one is not.
func TestFence_RefusesStartWhileNotArmed(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	t0 := e.clock.Now()
	e.renew(t, time.Hour, 1)
	e.create(t, "fenced", true, false)
	e.create(t, "plain", false, false)

	requireKind(t, e.m.Start(ctx, "fenced"), machine.ErrPreconditionFailed)
	_, alive := e.sup.Expiry("fenced")
	assert.False(t, alive, "no supervisor, so no VMM, while the fence is not armed")
	require.NoError(t, e.m.Start(ctx, "plain"))

	e.renewFence(t, "fenced", time.Minute, 1)
	require.NoError(t, e.m.Start(ctx, "fenced"))
	assert.Equal(t, t0.Add(time.Minute), e.expiry(t, "fenced"), "the fence is earlier than the node's")
}

// Renewing a Machine's fence moves only that Machine's expiry, with the same
// replay-safe numbers and a durable record.
func TestFence_RenewalMovesOnlyThatMachine(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	t0 := e.clock.Now()
	e.renew(t, time.Hour, 1)
	e.create(t, "a", true, false)
	e.create(t, "b", true, false)
	e.renewFence(t, "a", 10*time.Minute, 1)
	e.renewFence(t, "b", 10*time.Minute, 1)
	require.NoError(t, e.m.Start(ctx, "a"))
	require.NoError(t, e.m.Start(ctx, "b"))
	nodeBefore := e.status(t)

	st := e.renewFence(t, "a", 20*time.Minute, 2)
	assert.Equal(t, fencing.PhaseArmed, st.Phase)
	assert.Equal(t, 20*time.Minute, st.Remaining)
	assert.Equal(t, uint64(2), st.Renewal)
	assert.Equal(t, t0.Add(20*time.Minute), e.expiry(t, "a"))
	assert.Equal(t, t0.Add(10*time.Minute), e.expiry(t, "b"), "another Machine's expiry is not moved")
	assert.Equal(t, nodeBefore, e.status(t), "the node's deadline is not moved")
	rec, ok := e.fences.Recorded("a")
	require.True(t, ok)
	assert.Equal(t, t0.Add(20*time.Minute), rec.Expiry, "recorded before the call returned")

	// A shortening reaches the supervisor, and not above what is recorded.
	e.renewFence(t, "a", time.Minute, 3)
	assert.Equal(t, t0.Add(time.Minute), e.expiry(t, "a"))

	// A number that is not greater changes nothing.
	e.clock.Advance(10 * time.Second)
	st = e.renewFence(t, "a", time.Hour, 3)
	assert.Equal(t, 50*time.Second, st.Remaining)
	st = e.renewFence(t, "a", time.Hour, 2)
	assert.Equal(t, 50*time.Second, st.Remaining)
	assert.Equal(t, t0.Add(time.Minute), e.expiry(t, "a"))

	_, err := e.svc.RenewFence(ctx, "a", 0, 4)
	requireKind(t, err, machine.ErrInvalid)
}

// The renewal is refused for a Machine that has no fence of its own.
func TestFence_RenewalNeedsAFence(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	e.create(t, "plain", false, false)
	_, err := e.svc.RenewFence(ctx, "plain", time.Minute, 1)
	requireKind(t, err, machine.ErrPreconditionFailed)
	_, err = e.svc.InspectFence(ctx, "plain")
	requireKind(t, err, machine.ErrPreconditionFailed)
}

// A Machine's fence and the node's deadline both apply: its supervisor holds
// the earlier of the two, at each change of either.
func TestFence_TheEarlierExpiryEndsTheVMM(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	t0 := e.clock.Now()
	e.renew(t, time.Hour, 1)
	e.create(t, "m", true, false)
	e.renewFence(t, "m", 10*time.Minute, 1)
	require.NoError(t, e.m.Start(ctx, "m"))
	assert.Equal(t, t0.Add(10*time.Minute), e.expiry(t, "m"), "the fence is earlier")

	// The node's deadline shortened below the fence.
	e.renew(t, time.Minute, 2)
	assert.Equal(t, t0.Add(time.Minute), e.expiry(t, "m"), "the node's is earlier")

	// The node's extended again: the fence is what the supervisor holds.
	e.renew(t, time.Hour, 3)
	assert.Equal(t, t0.Add(10*time.Minute), e.expiry(t, "m"))

	// The fence extended beyond the node's: the node's is what it holds.
	e.renewFence(t, "m", 2*time.Hour, 2)
	assert.Equal(t, t0.Add(time.Hour), e.expiry(t, "m"))
	assert.Empty(t, e.store.Violations())

	// An mvmd restart brings it back to the same one.
	e.restart(t)
	assert.Equal(t, t0.Add(time.Hour), e.expiry(t, "m"))
}

// When a Machine's fence lapses its VMM is ended by its supervisor alone, and
// every other Machine keeps running. Inspect then names the fence and the time.
func TestFence_LapseEndsOnlyThatMachineAndInspectShowsIt(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	e.watch(t)
	e.renew(t, time.Hour, 1)
	e.create(t, "fenced", true, false)
	e.create(t, "other", true, false)
	e.create(t, "plain", false, false)
	e.renewFence(t, "fenced", time.Minute, 1)
	e.renewFence(t, "other", time.Hour, 1)
	require.NoError(t, e.m.Start(ctx, "fenced"))
	require.NoError(t, e.m.Start(ctx, "other"))
	require.NoError(t, e.m.Start(ctx, "plain"))

	lapse := e.wall.Now().Add(time.Minute)
	e.clock.Advance(time.Minute + 5*time.Second)
	e.wall.Set(lapse.Add(5 * time.Second))
	assert.Equal(t, []machine.MachineID{"fenced"}, e.sup.EndExpired(e.clock.Now()), "the supervisor's own count ends it")

	e.eventually(t, "the Machine is Stopped", func() bool { return e.state(t, "fenced") == machine.StateStopped })
	m := e.inspect(t, "fenced")
	assert.Contains(t, m.FailureReason, "own fence lapsed")
	assert.WithinDuration(t, lapse, m.FenceLapsedAt, time.Millisecond, "the time the fence lapsed, not the time it was found")
	assert.Equal(t, machine.StateRunning, e.state(t, "other"))
	assert.Equal(t, machine.StateRunning, e.state(t, "plain"))
	assert.Zero(t, e.inspect(t, "other").FenceLapsedAt)
}

// The finding holds when the VMM ended while mvmd was not running, and across a
// restart of mvmd after it.
func TestFence_LapseHoldsAcrossAnMvmdRestart(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	e.renew(t, time.Hour, 1)
	e.create(t, "fenced", true, false)
	e.create(t, "other", true, false)
	e.renewFence(t, "fenced", time.Minute, 1)
	e.renewFence(t, "other", time.Hour, 1)
	require.NoError(t, e.m.Start(ctx, "fenced"))
	require.NoError(t, e.m.Start(ctx, "other"))
	lapse := e.wall.Now().Add(time.Minute)

	// mvmd is down: the clock runs on and the supervisor ends the VMM.
	e.clock.Advance(2 * time.Minute)
	e.wall.Set(lapse.Add(time.Minute))
	require.Equal(t, []machine.MachineID{"fenced"}, e.sup.EndExpired(e.clock.Now()))

	e.restart(t)
	m := e.inspect(t, "fenced")
	assert.Equal(t, machine.StateStopped, m.State)
	assert.Contains(t, m.FailureReason, "own fence lapsed")
	assert.WithinDuration(t, lapse, m.FenceLapsedAt, time.Millisecond)
	assert.Equal(t, machine.StateRunning, e.state(t, "other"))

	// And again, with nothing new to find.
	e.restart(t)
	assert.WithinDuration(t, lapse, e.inspect(t, "fenced").FenceLapsedAt, time.Millisecond)
	assert.Contains(t, e.inspect(t, "fenced").FailureReason, "own fence lapsed")
}

// When the node's deadline passes before the Machine's fence, its stop is the
// end, and the Machine's reason does not name the fence.
func TestFence_TheNodesDeadlineFirstIsNotALapse(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	e.renew(t, time.Minute, 1)
	e.create(t, "m", true, false)
	e.renewFence(t, "m", time.Hour, 1)
	require.NoError(t, e.m.Start(ctx, "m"))
	e.clock.Advance(2 * time.Hour)
	require.Equal(t, []machine.MachineID{"m"}, e.sup.EndExpired(e.clock.Now()))

	e.restart(t)
	m := e.inspect(t, "m")
	assert.Equal(t, machine.StateStopped, m.State)
	assert.NotContains(t, m.FailureReason, "own fence")
	assert.Zero(t, m.FenceLapsedAt)
}

// A renewal after a lapse arms the fence again and starts nothing; a start of
// the Machine then clears the time.
func TestFence_RenewalAfterALapseStartsNothing(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	e.renew(t, 24*time.Hour, 1)
	e.create(t, "m", true, false)
	e.renewFence(t, "m", time.Minute, 1)
	require.NoError(t, e.m.Start(ctx, "m"))
	e.clock.Advance(2 * time.Minute)
	e.sup.EndExpired(e.clock.Now())
	e.restart(t)
	require.Equal(t, machine.StateStopped, e.state(t, "m"))
	requireKind(t, e.m.Start(ctx, "m"), machine.ErrPreconditionFailed)
	st, err := e.svc.InspectFence(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, fencing.PhaseExpired, st.Phase)

	st = e.renewFence(t, "m", time.Hour, 2)
	assert.Equal(t, fencing.PhaseArmed, st.Phase)
	assert.Equal(t, machine.StateStopped, e.state(t, "m"), "a renewal starts nothing")
	_, alive := e.sup.Expiry("m")
	assert.False(t, alive)
	require.NoError(t, e.m.Start(ctx, "m"))
	assert.Zero(t, e.inspect(t, "m").FenceLapsedAt)
}

// A host reboot leaves a fence not armed, so a Machine with restart_on_boot and
// a fence is not restarted until its fence is renewed, and it does not hold
// back the restart of another Machine (ADR 0062).
func TestFence_ARestartWaitsForTheMachinesFence(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	e.renew(t, time.Hour, 1)
	e.create(t, "fenced", true, true)
	e.create(t, "plain", false, true)
	e.renewFence(t, "fenced", time.Hour, 1)
	require.NoError(t, e.m.Start(ctx, "fenced"))
	require.NoError(t, e.m.Start(ctx, "plain"))

	// The host goes down: no supervisor survives.
	e.sup.EndExpired(fencing.BootTime(1 << 60))
	e.clock.Reboot("boot-2")
	e.restart(t)
	require.Equal(t, machine.StateStopped, e.state(t, "fenced"))
	require.Equal(t, machine.StateStopped, e.state(t, "plain"))
	assert.NotContains(t, e.inspect(t, "fenced").FailureReason, "own fence", "a reboot is not a lapse")

	e.watch(t)
	e.renew(t, time.Hour, 1)
	e.eventually(t, "the Machine without a fence is started", func() bool { return e.state(t, "plain") == machine.StateRunning })
	assert.Equal(t, machine.StateStopped, e.state(t, "fenced"), "its own fence is not armed")

	e.renewFence(t, "fenced", time.Hour, 1)
	e.eventually(t, "the fenced Machine is started once its fence is armed", func() bool { return e.state(t, "fenced") == machine.StateRunning })
}

// Delete Machine removes the fence, and a name made again starts unarmed.
func TestFence_DeleteReleasesIt(t *testing.T) {
	ctx := context.Background()
	e := newFenceEnv(t)
	e.renew(t, time.Hour, 1)
	e.create(t, "m", true, false)
	e.renewFence(t, "m", time.Minute, 1)
	_, ok := e.fences.Recorded("m")
	require.True(t, ok)
	require.NoError(t, e.m.Delete(ctx, "m"))
	_, ok = e.fences.Recorded("m")
	assert.False(t, ok)

	e.create(t, "m", true, false)
	st, err := e.svc.InspectFence(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, fencing.PhaseUnarmed, st.Phase)
}

// A node that keeps no fences refuses a Machine created with one.
func TestFence_CreateIsRefusedWithoutAStore(t *testing.T) {
	e := newEnv(t)
	s := spec()
	s.Fenced = true
	requireKind(t, e.m.Create(context.Background(), "m", "r", s), machine.ErrUnsupported)
}
