package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// startingInBackground runs Start on its own goroutine.
func (e *env) startingInBackground(id machine.MachineID) <-chan error {
	done := make(chan error, 1)
	go func() { done <- e.svc.Start(context.Background(), id) }()
	return done
}

// requireWaiting requires that the call has not returned yet.
func requireWaiting(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("the call returned while the guest was booting: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
}

// requireHeld requires the held call to have been entered, failing instead of
// hanging when it never was.
func requireHeld(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the guest was never sent its boot description")
	}
}

// requireDone returns the call's result, failing instead of hanging.
func requireDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("the call never returned")
		return nil
	}
}

// The Image Store holds the filesystem UUID and the run configuration of an
// image, and nothing of what it returns is shared with the store.
func TestImageStore_ReturnsTheUUIDAndTheRunConfiguration(t *testing.T) {
	e := newEnv()
	const uuid = "5b9e0b0e-4e3b-4a52-8d0e-2f6a8e1c9a11"
	want := func() machine.ImageRun {
		return machine.ImageRun{Entrypoint: []string{"/bin/sh"}, Cmd: []string{"-c", "serve"}, Env: map[string]string{"A": "1"}, WorkingDir: "/app", User: "app"}
	}
	e.images.ProvideWith("sha256:other", machine.LocatedImage{UUID: uuid, Run: want()})

	got, err := e.images.Locate(context.Background(), "sha256:other")
	require.NoError(t, err)
	assert.Equal(t, uuid, got.UUID)
	assert.Equal(t, want(), got.Run)
	assert.Equal(t, "/images/sha256:other.erofs", got.Path)

	got.Run.Entrypoint[0] = "changed"
	got.Run.Cmd[0] = "changed"
	got.Run.Env["A"] = "changed"
	again, err := e.images.Locate(context.Background(), "sha256:other")
	require.NoError(t, err)
	assert.Equal(t, want(), again.Run, "a caller cannot change what the store holds")

	// Control: an image the node does not hold has neither.
	_, err = e.images.Locate(context.Background(), "sha256:absent")
	require.ErrorIs(t, err, machine.ErrImageAbsent)
}

// Row "Start", column Effects: the guest is sent its boot description.
func TestStart_SendsTheGuestItsBootDescription(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.images.ProvideWith("sha256:one", machine.LocatedImage{UUID: "uuid-one", Run: machine.ImageRun{Entrypoint: []string{"/one"}, WorkingDir: "/1"}})
	e.images.ProvideWith("sha256:two", machine.LocatedImage{UUID: "uuid-two", Run: machine.ImageRun{Entrypoint: []string{"/two"}, Cmd: []string{"arg"}, Env: map[string]string{"K": "V"}}})
	for id, digest := range map[machine.MachineID]machine.ImageDigest{"a": "sha256:one", "b": "sha256:two"} {
		spec := baseSpec()
		spec.Image = digest
		require.NoError(t, e.svc.Create(ctx, id, request(id), spec))
		require.NoError(t, e.svc.Start(ctx, id))
	}

	for id, want := range map[machine.MachineID]machine.GuestBoot{
		"a": {ImageUUID: "uuid-one", Run: machine.ExecSpec{Command: []string{"/one"}, WorkingDir: "/1"}},
		"b": {ImageUUID: "uuid-two", Run: machine.ExecSpec{Command: []string{"/two", "arg"}, Env: map[string]string{"K": "V"}}},
	} {
		m := e.requireState(t, id, machine.StateRunning)
		require.NotEmpty(t, m.RootDiskUUID, "the setup: the root disk has a UUID")
		want.RootDiskUUID = m.RootDiskUUID
		want.Clock = e.clock
		want.Network = machine.GuestNetwork{Addresses: m.Network.Addresses, Gateways: m.Network.Gateways, Resolvers: m.Network.Resolvers}
		require.NotEmpty(t, want.Network.Gateways, "the setup: the Machine has a gateway to be told")
		assert.Equal(t, []machine.GuestBoot{want}, e.guest.Boots(id), "its own image's UUID and run configuration, its own root disk and network, sent once")
	}
	a, b := e.requireState(t, "a", machine.StateRunning), e.requireState(t, "b", machine.StateRunning)
	assert.NotEqual(t, a.RootDiskUUID, b.RootDiskUUID, "control: each Machine has a root disk of its own")
}

// The boot description carries the lifecycle's wall clock, so the guest's clock
// is set to the host's (ADR 0044), and a restart reads it again.
func TestStart_TheBootCallCarriesTheLifecyclesClockReading(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	first := time.Unix(1_700_000_000, 5)
	e.clock.Set(first)
	e.inState(t, "m", machine.StateStopped)
	require.NoError(t, e.svc.Start(ctx, "m"))
	e.clock.Set(first.Add(time.Hour))
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.Start(ctx, "m"))

	times := e.guest.BootTimes("m")
	require.Len(t, times, 3, "the setup: the Machine booted to be stopped, then twice more")
	assert.Equal(t, []int64{first.UnixNano(), first.Add(time.Hour).UnixNano()}, times[1:], "each Boot call carries the reading of the clock when it is made")
}

// A restart boots the guest again, as a new VMM holds a new guest.
func TestStart_ARestartSendsTheBootDescriptionAgain(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.inState(t, "m", machine.StateStopped)
	require.NoError(t, e.svc.Start(ctx, "m"))
	assert.Len(t, e.guest.Boots("m"), 2)

	// Control: a repeat of Start on a Running Machine sends nothing.
	require.NoError(t, e.svc.Start(ctx, "m"))
	assert.Len(t, e.guest.Boots("m"), 2)
}

// Restore does not boot: the restored guest has booted already, and is sent
// CatchUp instead (guest.md).
func TestRestoreSnapshot_DoesNotSendTheBootDescription(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	require.Len(t, e.guest.Boots("src"), 1, "the setup: the source was started")

	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
	e.requireState(t, "new", machine.StateRunning)
	assert.Empty(t, e.guest.Boots("new"))
	assert.Len(t, e.guest.CatchUps("new"), 1, "control: it was sent CatchUp")
}

// A Machine is not Running, nor reported so, until Boot returns, and a Start
// repeated meanwhile waits for that outcome and sends nothing more.
func TestStart_IsRunningOnlyOnceTheEntrypointRuns(t *testing.T) {
	e := newEnv()
	e.created(t, "m")
	entered, release := e.guest.Hold("boot")
	first := e.startingInBackground("m")
	requireHeld(t, entered)

	m, err := e.svc.Inspect(context.Background(), "m")
	require.NoError(t, err)
	assert.Equal(t, machine.StateCreated, m.State, "the VMM runs and the entrypoint does not")
	assert.NotContains(t, e.events.States("m"), machine.StateRunning)

	second := e.startingInBackground("m")
	requireWaiting(t, first)
	requireWaiting(t, second)

	release()
	require.NoError(t, requireDone(t, first))
	require.NoError(t, requireDone(t, second), "the repeat has the outcome of the start in progress")
	e.requireState(t, "m", machine.StateRunning)
	assert.Equal(t, 1, e.guest.Calls("boot"))
	assert.Equal(t, 1, e.sup.Calls("start"), "one VMM process")
}

// boundedEnv is an env whose lifecycle waits at most d for a guest's boot.
func boundedEnv(d time.Duration) *env {
	e := newEnv()
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels()), machine.WithBootTimeout(d))
	return e
}

// bootBound is the bound the tests below put on the guest's boot. No clock in
// the package makes a timer, so the bound is real and short.
const bootBound = 150 * time.Millisecond

// A guest that never finishes booting does not keep the Machine for ever: when
// the bound passes the VMM is ended and the Machine is Stopped with the reason.
func TestStart_AGuestThatNeverFinishesBootingEndsStoppedAfterTheBound(t *testing.T) {
	for _, from := range []machine.State{machine.StateCreated, machine.StateStopped} {
		t.Run("from "+from.String(), func(t *testing.T) {
			e := boundedEnv(bootBound)
			e.inState(t, "m", from)
			entered, release := e.guest.Hold("boot")
			defer release()
			begun := time.Now()
			done := e.startingInBackground("m")
			requireHeld(t, entered)

			err := requireDone(t, done)
			requireKind(t, err, machine.ErrFailed)
			assert.GreaterOrEqual(t, time.Since(begun), bootBound, "not before the bound")
			assert.Contains(t, err.Error(), "the guest did not finish booting within "+bootBound.String())
			m := e.requireState(t, "m", machine.StateStopped)
			e.requireStored(t, "m", machine.StateStopped)
			assert.Contains(t, m.FailureReason, "the guest did not finish booting within "+bootBound.String())
			_, running := e.sup.Running("m")
			assert.False(t, running, "the VMM process was ended")
			_, applied := e.net.Applied("m")
			assert.False(t, applied, "the tap was removed")
		})
	}
}

// A Stop sent while the guest is booting waits for the Machine's lock, and
// completes once the bound has passed and Start has let go of it.
func TestStart_AStopSentWhileTheGuestBootsCompletesOnceTheBoundPasses(t *testing.T) {
	e := boundedEnv(bootBound)
	e.created(t, "m")
	entered, release := e.guest.Hold("boot")
	defer release()
	start := e.startingInBackground("m")
	requireHeld(t, entered)

	stop := make(chan error, 1)
	go func() { stop <- e.svc.Stop(context.Background(), "m", time.Second) }()

	requireKind(t, requireDone(t, start), machine.ErrFailed)
	require.NoError(t, requireDone(t, stop), "Stop of a Machine that is Stopped succeeds")
	e.requireState(t, "m", machine.StateStopped)
	_, running := e.sup.Running("m")
	assert.False(t, running)
}

// Control: a guest that finishes within the bound reaches Running, and the
// caller's context ending meanwhile does not end the boot.
func TestStart_AGuestThatFinishesWithinTheBoundReachesRunning(t *testing.T) {
	t.Run("a slow boot", func(t *testing.T) {
		e := boundedEnv(5 * time.Second)
		e.created(t, "m")
		entered, release := e.guest.Hold("boot")
		done := e.startingInBackground("m")
		requireHeld(t, entered)
		release()

		require.NoError(t, requireDone(t, done))
		e.requireState(t, "m", machine.StateRunning)
	})

	t.Run("the caller gives up meanwhile", func(t *testing.T) {
		e := boundedEnv(5 * time.Second)
		e.created(t, "m")
		entered, release := e.guest.Hold("boot")
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- e.svc.Start(ctx, "m") }()
		requireHeld(t, entered)
		cancel()
		release()

		require.NoError(t, requireDone(t, done), "the boot is bounded by the node's timeout and not by the caller")
		e.requireState(t, "m", machine.StateRunning)
	})
}

// A Boot that fails ends Start in the state the row names, with the step in
// the reason.
func TestStart_AFailedGuestBootEndsStoppedWithTheStepInTheReason(t *testing.T) {
	ctx := context.Background()
	for _, from := range []machine.State{machine.StateCreated, machine.StateStopped} {
		t.Run("from "+from.String(), func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", from)
			e.guest.FailAt("boot", &machine.BootStepError{Step: "mount-image", Detail: "no device carries the UUID"})

			err := e.svc.Start(ctx, "m")
			requireKind(t, err, machine.ErrFailed)
			assert.Contains(t, err.Error(), "mount-image")
			assert.Contains(t, err.Error(), "no device carries the UUID")
			m := e.requireState(t, "m", machine.StateStopped)
			e.requireStored(t, "m", machine.StateStopped)
			assert.Contains(t, m.FailureReason, "mount-image", "Inspect reports the step")
			assert.Contains(t, m.FailureReason, "no device carries the UUID")
			assert.NotContains(t, m.FailureReason, "not reachable", "the agent answered")
			_, running := e.sup.Running("m")
			assert.False(t, running, "the VMM process was ended")
			_, applied := e.net.Applied("m")
			assert.False(t, applied, "the tap was removed")
			assert.NotContains(t, e.events.States("m")[len(e.events.States("m"))-1:], machine.StateRunning)

			// Control: the same Machine boots once the guest stops failing.
			e.guest.Heal("boot")
			require.NoError(t, e.svc.Start(ctx, "m"))
			assert.Empty(t, e.requireState(t, "m", machine.StateRunning).FailureReason)
		})
	}

	t.Run("a failed boot whose VMM cannot be ended is Faulted", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.guest.FailAt("boot", &machine.BootStepError{Step: "overlay", Detail: "x"})
		e.sup.FailAt("terminate", errBoom)

		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		m := e.requireState(t, "m", machine.StateFaulted)
		assert.Contains(t, m.FailureReason, "overlay")
		e.requireStored(t, "m", machine.StateFaulted)
	})
}

// A Boot that did not reach the guest is a failed Start too: whether the guest
// began to boot is not known, and ending the VMM settles it.
func TestStart_ABootThatDoesNotReachTheGuestEndsStopped(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.created(t, "m")
	e.guest.FailAt("boot", errBoom)

	err := e.svc.Start(ctx, "m")
	requireKind(t, err, machine.ErrFailed)
	m := e.requireState(t, "m", machine.StateStopped)
	assert.Contains(t, m.FailureReason, "not reachable")
	assert.NotContains(t, m.FailureReason, "mount-image")
	_, running := e.sup.Running("m")
	assert.False(t, running, "the VMM process was ended")
	assert.Empty(t, e.guest.Boots("m"), "the guest was not told")

	// Control: reachable again, the same Machine starts.
	e.guest.Heal("boot")
	require.NoError(t, e.svc.Start(ctx, "m"))
	e.requireState(t, "m", machine.StateRunning)
	assert.Len(t, e.guest.Boots("m"), 1)
}

// Nothing is sent to a guest whose VMM did not start.
func TestStart_TheGuestIsNotSentBootBeforeTheVMMRuns(t *testing.T) {
	ctx := context.Background()
	for _, step := range []string{"describe", "start", "boot"} {
		e := newEnv()
		e.created(t, "m")
		if step == "describe" || step == "boot" {
			e.driver.FailAt(step, errBoom)
		} else {
			e.sup.FailAt(step, errBoom)
		}

		requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		assert.Zero(t, e.guest.Calls("boot"), "step %s failed first", step)
	}

	// Control: with nothing failing the guest is sent it.
	e := newEnv()
	e.created(t, "m")
	require.NoError(t, e.svc.Start(ctx, "m"))
	assert.Equal(t, 1, e.guest.Calls("boot"))
}
