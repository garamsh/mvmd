package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// attachedRunning is a Running Machine "m" with disk "d" attached to its VMM,
// found by its guest and recorded: the state every death below departs from.
func attachedRunning(t *testing.T) *env {
	t.Helper()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("d")
	e.inState(t, "m", machine.StateRunning)
	require.NoError(t, e.svc.AttachDisk(context.Background(), "m", rw("d")))
	return e
}

// A hot attach records the holder, asks the Driver, then has the guest find the
// disk (ADR 0023). Each case is the state a death after one step leaves.
func TestRestore_ARunningMachineHoldsOnlyDisksTheVMMAndTheGuestAgreeOn(t *testing.T) {
	ctx := context.Background()

	t.Run("a death after the record: the VMM lacks the disk, so the record drops it", func(t *testing.T) {
		e := attachedRunning(t)
		require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
		require.NoError(t, e.driver.DetachDisk(ctx, "m", "d"))
		e.created(t, "other")

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, machine.StateRunning, m.State)
		assert.Empty(t, m.Disks)
		require.NoError(t, svc.AttachDisk(ctx, "other", rw("d")), "the holder is released")
		rec, _ := e.store.Record("m")
		assert.Empty(t, rec.Machine.Disks, "the finding is recorded")
		assert.Empty(t, e.driver.HotDisks("m"), "nothing was attached by the restart")
	})

	t.Run("a death after the record: the dropped disk can be attached again", func(t *testing.T) {
		e := attachedRunning(t)
		require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
		require.NoError(t, e.driver.DetachDisk(ctx, "m", "d"))

		svc := e.reopened(t)
		require.NoError(t, svc.AttachDisk(ctx, "m", rw("d")))
		assert.Equal(t, []machine.DiskID{"d"}, e.driver.HotDisks("m"))
		assert.True(t, e.guest.DiskFound("m", "d"))
	})

	t.Run("a death after the Driver attach: the guest finds the disk and the Machine stays Running", func(t *testing.T) {
		e := attachedRunning(t)
		require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
		require.False(t, e.guest.DiskFound("m", "d"))

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, machine.StateRunning, m.State)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, m.Disks)
		assert.True(t, e.guest.DiskFound("m", "d"), "the guest found it on restart")
		assert.Equal(t, []machine.DiskID{"d"}, e.driver.HotDisks("m"), "nothing was unplugged")
	})

	t.Run("a death after the Driver attach: a guest that cannot find the disk Faults the Machine, naming it", func(t *testing.T) {
		e := attachedRunning(t)
		require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
		e.guest.FailAt("find-disk", errBoom)

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, machine.StateFaulted, m.State)
		assert.Contains(t, m.FailureReason, `"d"`)
		e.requireStored(t, "m", machine.StateFaulted)
		assert.Equal(t, []machine.DiskID{"d"}, e.driver.HotDisks("m"), "nothing was unplugged")
	})

	t.Run("a death after the guest find: nothing changes", func(t *testing.T) {
		e := attachedRunning(t)
		e.created(t, "other")

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, machine.StateRunning, m.State)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, m.Disks)
		assert.True(t, e.guest.DiskFound("m", "d"))
		requireKind(t, svc.AttachDisk(ctx, "other", rw("d")), machine.ErrPreconditionFailed)
	})

	t.Run("a VMM whose drives cannot be read Faults the Machine, naming the disk", func(t *testing.T) {
		e := attachedRunning(t)
		e.driver.FailAt("drive", errBoom)

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, machine.StateFaulted, m.State)
		assert.Contains(t, m.FailureReason, `"d"`)
		e.requireStored(t, "m", machine.StateFaulted)
	})

	t.Run("a disk given at boot is kept and not asked of the guest", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.created(t, "m")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.svc.Start(ctx, "m"))

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, machine.StateRunning, m.State)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, m.Disks)
		assert.Zero(t, e.guest.Calls("find-disk"))
	})

	t.Run("a Machine that is not Running is not checked", func(t *testing.T) {
		e := attachedRunning(t)
		require.NoError(t, e.svc.Pause(ctx, "m"))
		callsBefore := e.driver.Calls("drive")

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, machine.StatePaused, m.State)
		assert.Equal(t, callsBefore, e.driver.Calls("drive"))
	})
}

// A Machine restored from a snapshot answers the Driver's Drive as a booted one
// does: the Driver restored it, so it knows the drives the restore gave it.
func TestRestore_ARestoredMachineHoldsOnlyDisksItsVMMHas(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilitySnapshot, machine.CapabilityDiskHotplug)
	e.snapshotted(t, "src", machine.StateRunning)
	require.NoError(t, e.svc.RestoreSnapshot(ctx, "m", "r-m", "snap", narrowPolicy))
	e.requireState(t, "m", machine.StateRunning)
	require.Empty(t, e.driver.BootedWith("m").Devices, "the setup: the Driver never booted it")
	require.NotEmpty(t, e.driver.RestoredWith("m").Devices, "the setup: the Driver restored it")
	e.disks.Provide("d")
	e.created(t, "other")
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))

	// A death after the record: the VMM and the guest lack the disk.
	require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
	require.NoError(t, e.driver.DetachDisk(ctx, "m", "d"))

	svc := e.reopened(t)
	m, err := svc.Inspect(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, machine.StateRunning, m.State)
	assert.Empty(t, m.Disks, "the record dropped the disk the VMM lacks")
	require.NoError(t, svc.AttachDisk(ctx, "other", rw("d")), "the holder is released")

	// Control: the restored Machine's own boot drives are kept.
	state, err := e.driver.Drive(ctx, "m", m.RootDisk)
	require.NoError(t, err)
	assert.Equal(t, machine.DriveAtBoot, state)
}

// A Machine the Driver neither booted nor restored is one a new process
// inherited with its VMM. The in-memory Driver does not carry the VMM over, so
// it reports the disks the record lists as given at boot, and the restart
// keeps them.
func TestRestore_AnInheritedMachineKeepsItsDisks(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("d")
	e.created(t, "m")
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
	require.NoError(t, e.svc.Start(ctx, "m"))

	e.driver = memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: []machine.Capability{machine.CapabilityDiskHotplug}})
	require.Empty(t, e.driver.BootedWith("m").Devices, "the setup: the new Driver never booted it")
	require.Empty(t, e.driver.RestoredWith("m").Devices, "the setup: the new Driver never restored it")

	svc := e.reopened(t)
	m, err := svc.Inspect(ctx, "m")
	require.NoError(t, err)
	assert.Equal(t, machine.StateRunning, m.State)
	assert.Equal(t, []machine.DiskAttachment{rw("d")}, m.Disks)
	assert.Equal(t, 1, e.driver.Calls("drive"), "control: the Driver was asked")
	assert.Zero(t, e.guest.Calls("find-disk"))
}
