package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// createdWithDisks is a Created Machine "m" naming disks "a" and "b", on a
// Driver that can hot-plug.
func createdWithDisks(t *testing.T) *env {
	t.Helper()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("a")
	e.disks.Provide("b")
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{rw("a"), ro("b")}
	require.NoError(t, e.svc.Create(context.Background(), "m", request("m"), spec))
	return e
}

// ADR 0041 decision 5: Create takes a disk only on a Driver that can hot-plug,
// since Start puts it on the VMM that way.
func TestCreate_ADiskNeedsADriverThatCanHotPlug(t *testing.T) {
	ctx := context.Background()
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{rw("a")}

	e := newEnv()
	e.disks.Provide("a")
	requireKind(t, e.svc.Create(ctx, "m", request("m"), spec), machine.ErrUnsupported)
	e.requireNoMachine(t, "m")
	e.requireNoDiskHolder(t, "a")

	// Control: the same call on a Driver that can hot-plug succeeds, so the
	// refusal was the capability and nothing else.
	e = newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("a")
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec))

	// Control: a Machine with no disk needs no capability.
	require.NoError(t, newEnv().svc.Create(ctx, "m", request("m"), baseSpec()))
}

// Start gives the VMM the image and the root disk, then has each named disk
// put on the running VMM and found by the guest before it sends Boot.
func TestStart_EachNamedDiskIsFoundByTheGuestBeforeBoot(t *testing.T) {
	ctx := context.Background()
	e := createdWithDisks(t)
	// A boot the guest refuses leaves what was done before it to be read.
	e.guest.FailAt("boot", errBoom)

	requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
	assert.Equal(t, []machine.DiskID{"a", "b"}, e.driver.HotDisks("m"))
	assert.True(t, e.guest.DiskFound("m", "a"))
	assert.True(t, e.guest.DiskFound("m", "b"))
	assert.Equal(t, 1, e.guest.Calls("boot"), "setup: the guest was sent Boot")

	// Control: with the guest accepting Boot the same Machine runs.
	e.guest.Heal("boot")
	require.NoError(t, e.svc.Start(ctx, "m"))
	e.requireState(t, "m", machine.StateRunning)
}

// A disk that cannot be attached or found fails the Start as a failed boot
// does: the VMM is ended, the Machine is Stopped with a reason naming the disk,
// and the guest is never sent Boot.
func TestStart_ADiskThatFailsEndsTheVMMAndNamesTheDisk(t *testing.T) {
	ctx := context.Background()
	tests := []struct {
		name string
		fail func(e *env)
		heal func(e *env)
	}{
		{"the Driver cannot attach it", func(e *env) { e.driver.FailAfter("attach", 1, errBoom) }, func(e *env) { e.driver.Heal("attach") }},
		{"the guest cannot find it", func(e *env) { e.guest.FailAfter("find-disk", 1, errBoom) }, func(e *env) { e.guest.Heal("find-disk") }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := createdWithDisks(t)
			tt.fail(e)

			err := e.svc.Start(ctx, "m")
			requireKind(t, err, machine.ErrFailed)
			m := e.requireState(t, "m", machine.StateStopped)
			assert.Contains(t, m.FailureReason, `disk "b"`, "the second disk is the one that failed")
			assert.Contains(t, m.FailureReason, "injected failure")
			assert.Zero(t, e.guest.Calls("boot"), "the guest was never sent Boot")
			requireUnwound(t, e, "m")

			// Control: the same Machine starts once the step stops failing.
			tt.heal(e)
			require.NoError(t, e.svc.Start(ctx, "m"))
			e.requireState(t, "m", machine.StateRunning)
		})
	}
}

// The per-disk rescans are inside the boot timeout.
func TestStart_ADiskTheGuestNeverFindsFailsAfterTheBootTimeout(t *testing.T) {
	ctx := context.Background()
	e := createdWithDisks(t)
	svc := e.reopenedWith(t, machine.WithBootTimeout(bootBound))
	entered, release := e.guest.Hold("find-disk")
	defer release()

	begun := time.Now()
	done := make(chan error, 1)
	go func() { done <- svc.Start(ctx, "m") }()
	requireHeld(t, entered)

	err := requireDone(t, done)
	requireKind(t, err, machine.ErrFailed)
	assert.GreaterOrEqual(t, time.Since(begun), bootBound, "not before the bound")
	assert.Contains(t, err.Error(), "the guest did not finish booting within "+bootBound.String())
	requireUnwound(t, e, "m")
}

// ADR 0041 decision 5: a disk named at Create has a guest record, so a Running
// Machine detaches it as it detaches one attached later.
func TestDetachDisk_ADiskNamedAtCreateIsDetachedFromARunningMachine(t *testing.T) {
	ctx := context.Background()
	e := createdWithDisks(t)
	require.NoError(t, e.svc.Start(ctx, "m"))
	require.Equal(t, []machine.DiskID{"a", "b"}, e.driver.HotDisks("m"), "setup: both disks are on the VMM")

	require.NoError(t, e.svc.DetachDisk(ctx, "m", "a"))
	assert.Equal(t, []machine.DiskID{"b"}, e.driver.HotDisks("m"))
	assert.False(t, e.guest.DiskFound("m", "a"))
	assert.Equal(t, []machine.DiskAttachment{ro("b")}, attached(t, e, "m"))
	e.requireState(t, "m", machine.StateRunning)
	e.requireNoDiskHolder(t, "a")
}

// ADR 0041 decision 6: after mvmd restarts, a create-time disk is a drive added
// since boot, so the guest is asked to find it again and the record keeps it.
func TestRestart_ADiskNamedAtCreateIsFoundByTheGuestAgain(t *testing.T) {
	ctx := context.Background()
	e := createdWithDisks(t)
	require.NoError(t, e.svc.Start(ctx, "m"))
	state, err := e.driver.Drive(ctx, "m", "a")
	require.NoError(t, err)
	require.Equal(t, machine.DriveHotPlugged, state, "setup: the VMM reports the disk as added since boot")
	before := e.guest.Calls("find-disk")

	svc := e.reopened(t)
	m, err := svc.Inspect(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, machine.StateRunning, m.State)
	assert.Equal(t, []machine.DiskAttachment{rw("a"), ro("b")}, m.Disks)
	assert.Equal(t, before+2, e.guest.Calls("find-disk"), "the guest was asked for each disk again")
}
