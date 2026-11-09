package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
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
	want := func() machine.ExecSpec {
		return machine.ExecSpec{Command: []string{"/bin/sh", "-c", "serve"}, Env: map[string]string{"A": "1"}, WorkingDir: "/app"}
	}
	e.images.ProvideWith("sha256:other", machine.LocatedImage{UUID: uuid, Run: want()})

	got, err := e.images.Locate(context.Background(), "sha256:other")
	require.NoError(t, err)
	assert.Equal(t, uuid, got.UUID)
	assert.Equal(t, want(), got.Run)
	assert.Equal(t, "/images/sha256:other.erofs", got.Path)

	got.Run.Command[0] = "changed"
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
	e.images.ProvideWith("sha256:one", machine.LocatedImage{UUID: "uuid-one", Run: machine.ExecSpec{Command: []string{"/one"}, WorkingDir: "/1"}})
	e.images.ProvideWith("sha256:two", machine.LocatedImage{UUID: "uuid-two", Run: machine.ExecSpec{Command: []string{"/two", "arg"}, Env: map[string]string{"K": "V"}}})
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
		want.Network = machine.GuestNetwork{Addresses: m.Network.Addresses, Gateways: m.Network.Gateways, Resolvers: m.Network.Resolvers}
		require.NotEmpty(t, want.Network.Gateways, "the setup: the Machine has a gateway to be told")
		assert.Equal(t, []machine.GuestBoot{want}, e.guest.Boots(id), "its own image's UUID and run configuration, its own root disk and network, sent once")
	}
	a, b := e.requireState(t, "a", machine.StateRunning), e.requireState(t, "b", machine.StateRunning)
	assert.NotEqual(t, a.RootDiskUUID, b.RootDiskUUID, "control: each Machine has a root disk of its own")
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
