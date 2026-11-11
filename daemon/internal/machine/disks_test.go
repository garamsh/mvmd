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

func rw(disk machine.DiskID) machine.DiskAttachment { return machine.DiskAttachment{Disk: disk} }
func ro(disk machine.DiskID) machine.DiskAttachment {
	return machine.DiskAttachment{Disk: disk, ReadOnly: true}
}

// blindGuest answers a find of a disk and records nothing of it.
type blindGuest struct{ machine.Guest }

func (blindGuest) FindDisk(context.Context, machine.MachineID, machine.DiskID) error { return nil }

func attached(t *testing.T, e *env, id machine.MachineID) []machine.DiskAttachment {
	t.Helper()
	m, err := e.svc.Inspect(context.Background(), id)
	require.NoError(t, err)
	return m.Disks
}

// Row "Attach disk", column Needs.
func TestAttachDisk_Needs(t *testing.T) {
	ctx := context.Background()

	t.Run("a disk the node holds", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.created(t, "m")
		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("missing")), machine.ErrNotFound)
		assert.Empty(t, attached(t, e, "m"))

		e.disks.Provide("missing") // Control: held, it attaches.
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("missing")))
	})

	t.Run("a Machine that exists", func(t *testing.T) {
		requireKind(t, newEnv().svc.AttachDisk(ctx, "absent", rw("d")), machine.ErrNotFound)
	})

	// One holder per disk (ADR 0009): a second writer is refused, and so is
	// any holder beside a writer or beside a reader.
	holders := []struct {
		name         string
		first, other machine.DiskAttachment
	}{
		{"a second writer", rw("d"), rw("d")},
		{"a writer beside a reader", ro("d"), rw("d")},
		{"a reader beside a writer", rw("d"), ro("d")},
		{"a reader beside a reader", ro("d"), ro("d")},
	}
	for _, tt := range holders {
		t.Run("refuses "+tt.name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			e.disks.Provide("d")
			e.created(t, "first")
			e.created(t, "other")
			require.NoError(t, e.svc.AttachDisk(ctx, "first", tt.first))

			requireKind(t, e.svc.AttachDisk(ctx, "other", tt.other), machine.ErrPreconditionFailed)
			assert.Empty(t, attached(t, e, "other"))
			assert.Equal(t, []machine.DiskAttachment{tt.first}, attached(t, e, "first"))

			// Control: once the holder releases the disk, the same call is accepted.
			require.NoError(t, e.svc.DetachDisk(ctx, "first", "d"))
			require.NoError(t, e.svc.AttachDisk(ctx, "other", tt.other))
		})
	}

	t.Run("refuses another Machine's root disk", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.created(t, "m")
		e.created(t, "other")
		requireKind(t, e.svc.AttachDisk(ctx, "other", ro("m-root")), machine.ErrPreconditionFailed)
	})

	t.Run("hotplug capability if Running", func(t *testing.T) {
		e := newEnv() // a Driver without the capability
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrUnsupported)
		assert.Empty(t, attached(t, e, "m"))
		assert.Zero(t, e.driver.Calls("attach"))

		with := newEnv(machine.CapabilityDiskHotplug)
		with.disks.Provide("d")
		with.inState(t, "m", machine.StateRunning)
		require.NoError(t, with.svc.AttachDisk(ctx, "m", rw("d")))
	})

	t.Run("a Machine that is not Paused, because its guest cannot find the disk", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StatePaused)
		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrPreconditionFailed)
		assert.Empty(t, attached(t, e, "m"))
		assert.Zero(t, e.driver.Calls("attach"))
		assert.Zero(t, e.guest.Calls("find-disk"))

		// Control: resumed, the same call is accepted.
		require.NoError(t, e.svc.Resume(ctx, "m"))
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
	})

	// Start plugs every attached disk (ADR 0041), so a Driver that cannot
	// hot-plug takes no attach in any state, or every later Start would fail.
	t.Run("hotplug capability if Created or Stopped", func(t *testing.T) {
		for _, state := range []machine.State{machine.StateCreated, machine.StateStopped} {
			e := newEnv() // a Driver without the capability
			e.disks.Provide("d")
			e.inState(t, "m", state)
			requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrUnsupported)
			assert.Empty(t, attached(t, e, "m"), state)

			with := newEnv(machine.CapabilityDiskHotplug)
			with.disks.Provide("d")
			with.inState(t, "m", state)
			require.NoError(t, with.svc.AttachDisk(ctx, "m", rw("d")), state)
			assert.Zero(t, with.guest.Calls("find-disk"), "there is no guest to ask")
		}
	})
}

// Row "Attach disk", column On success.
func TestAttachDisk_OnSuccess(t *testing.T) {
	ctx := context.Background()

	t.Run("a Machine without a VMM records the disk for its next boot", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.created(t, "m")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", ro("d")))

		assert.Equal(t, []machine.DiskAttachment{ro("d")}, attached(t, e, "m"))
		rec, _ := e.store.Record("m")
		assert.Equal(t, []machine.DiskAttachment{ro("d")}, rec.Machine.Disks)
		assert.Zero(t, e.driver.Calls("attach"), "there is no VMM to ask")
	})

	t.Run("a Running Machine gets it from the Driver", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))

		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
		assert.Equal(t, []machine.DiskID{"d"}, e.driver.HotDisks("m"))
	})
}

// Row "Attach disk", column Repeat.
func TestAttachDisk_Repeat(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("d")
	e.inState(t, "m", machine.StateRunning)
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))

	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")), "same parameters: success")
	assert.Equal(t, 1, e.driver.Calls("attach"), "the Driver was not asked again")
	assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))

	requireKind(t, e.svc.AttachDisk(ctx, "m", ro("d")), machine.ErrConflict)
	assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"), "other parameters change nothing")
}

// Row "Attach disk", column State after failure.
func TestAttachDisk_StateAfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("the Driver fails and the disk is not attached or held", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.created(t, "other")
		e.driver.FailAt("attach", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		assert.Empty(t, attached(t, e, "m"))
		rec, _ := e.store.Record("m")
		assert.Empty(t, rec.Machine.Disks, "the holder record was undone")
		require.NoError(t, e.svc.AttachDisk(ctx, "other", rw("d")), "so another Machine can hold it")
	})

	t.Run("the holder cannot be recorded, so the Driver is not asked", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.store.FailAt("save", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		assert.Empty(t, attached(t, e, "m"))
		assert.Zero(t, e.driver.Calls("attach"))
		e.requireState(t, "m", machine.StateRunning)
	})

	t.Run("the Driver fails and the undo cannot be recorded, so the Machine is Faulted", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.driver.FailAt("attach", errBoom)
		e.store.FailAfter("save", e.store.Calls("save")+1, errBoom) // the holder record is saved; its undo is not

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
	})
}

// Row "Detach disk", column Needs.
func TestDetachDisk_Needs(t *testing.T) {
	ctx := context.Background()

	t.Run("hotplug capability if Running", func(t *testing.T) {
		// The disk is attached while the Machine is Created, under a Driver
		// that could hot-plug, then the node restarts with one that cannot
		// and starts the Machine.
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.created(t, "m")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.driver = memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a"})
		svc := e.reopened(t)
		require.NoError(t, svc.Start(ctx, "m"))

		requireKind(t, svc.DetachDisk(ctx, "m", "d"), machine.ErrUnsupported)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
		assert.Zero(t, e.driver.Calls("detach"))
		assert.Zero(t, e.guest.Calls("release-disk"))
	})

	t.Run("a Machine that is not Paused, because its guest cannot release the disk", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.svc.Pause(ctx, "m"))

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrPreconditionFailed)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
		assert.Zero(t, e.driver.Calls("detach"))
		assert.Zero(t, e.guest.Calls("release-disk"))

		// Control: resumed, the same call is accepted.
		require.NoError(t, e.svc.Resume(ctx, "m"))
		require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))
	})

	t.Run("a Created or Stopped Machine needs no capability", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.created(t, "m")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))
	})

	t.Run("the root disk is held for the Machine's life", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.created(t, "m")
		requireKind(t, e.svc.DetachDisk(ctx, "m", "m-root"), machine.ErrPreconditionFailed)
	})

	requireKind(t, newEnv().svc.DetachDisk(ctx, "absent", "d"), machine.ErrNotFound)
}

// Row "Detach disk", column On success.
func TestDetachDisk_OnSuccess(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("d")
	e.inState(t, "m", machine.StateRunning)
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))

	require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))
	assert.Empty(t, attached(t, e, "m"))
	assert.Empty(t, e.driver.HotDisks("m"))
	rec, _ := e.store.Record("m")
	assert.Empty(t, rec.Machine.Disks)
	_, held := e.disks.Holds("d")
	assert.True(t, held, "detaching does not delete the disk")
}

// Row "Detach disk", column Repeat.
func TestDetachDisk_Repeat(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.Provide("d")
	e.inState(t, "m", machine.StateRunning)
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
	require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))

	require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"), "absent: success")
	require.NoError(t, e.svc.DetachDisk(ctx, "m", "never-attached"))
	assert.Equal(t, 1, e.driver.Calls("detach"), "the Driver was not asked again")
}

// Row "Detach disk", column State after failure.
func TestDetachDisk_StateAfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("the Driver fails and the disk is still attached and held", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.created(t, "other")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.driver.FailAt("detach", errBoom)

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
		requireKind(t, e.svc.AttachDisk(ctx, "other", rw("d")), machine.ErrPreconditionFailed)
	})

	t.Run("the release cannot be recorded on a Machine without a VMM", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.created(t, "m")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.store.FailAt("save", errBoom)

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
		e.requireState(t, "m", machine.StateCreated)
	})
}

// The guest's part in a hot attach and detach (ADR 0022).
func TestAttachDisk_TheGuestFindsTheDisk(t *testing.T) {
	ctx := context.Background()

	t.Run("a Running Machine's guest finds the disk after the Driver attaches it", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))

		assert.Equal(t, 1, e.guest.Calls("find-disk"))
		assert.True(t, e.guest.DiskFound("m", "d"))

		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		assert.Equal(t, 2, e.guest.Calls("find-disk"), "a repeat has the guest find the disk again, which is repeatable")
		assert.Equal(t, 1, e.driver.Calls("attach"), "a repeat does not attach the disk again")
	})

	t.Run("a guest that cannot find it leaves the disk detached and not held", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.created(t, "other")
		e.guest.FailAt("find-disk", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		assert.Empty(t, attached(t, e, "m"))
		assert.Empty(t, e.driver.HotDisks("m"), "the Driver took the disk off again")
		rec, _ := e.store.Record("m")
		assert.Empty(t, rec.Machine.Disks)
		e.requireState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "other", rw("d")), "so another Machine can hold it")
	})

	t.Run("a guest that cannot find it and a Driver that cannot detach it Fault the Machine", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("find-disk", errBoom)
		e.driver.FailAt("detach", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		e.requireState(t, "m", machine.StateFaulted)
	})
}

func TestDetachDisk_TheGuestReleasesTheDiskFirst(t *testing.T) {
	ctx := context.Background()
	running := func(t *testing.T) *env {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		return e
	}

	t.Run("the guest releases it and then the Driver detaches it", func(t *testing.T) {
		e := running(t)
		require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))

		assert.Equal(t, 1, e.guest.Calls("release-disk"))
		assert.False(t, e.guest.DiskFound("m", "d"))
		assert.Empty(t, e.driver.HotDisks("m"))
		assert.Empty(t, attached(t, e, "m"))
	})

	t.Run("a guest that is using it refuses, and the Driver is not asked", func(t *testing.T) {
		e := running(t)
		e.guest.HoldDisk("m", "d", true)

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrPreconditionFailed)
		assert.Zero(t, e.driver.Calls("detach"))
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
		assert.Equal(t, []machine.DiskID{"d"}, e.driver.HotDisks("m"))

		// Control: once the guest lets go, the same call is accepted.
		e.guest.HoldDisk("m", "d", false)
		require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))
	})

	t.Run("a guest that does not answer leaves the disk attached", func(t *testing.T) {
		e := running(t)
		e.guest.FailAt("release-disk", errBoom)

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		assert.Zero(t, e.driver.Calls("detach"))
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
		e.requireState(t, "m", machine.StateRunning)
	})

	t.Run("a disk the guest never found is refused, and a Stopped Machine detaches it", func(t *testing.T) {
		// A disk on a VMM started before ADR 0041 was never found by its guest,
		// as a guest that records nothing of a find shows. Control: a disk named
		// at Create and started now is detached from a Running Machine
		// (TestDetachDisk_ADiskNamedAtCreateIsDetachedFromARunningMachine).
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, blindGuest{e.guest}, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels()))
		spec := baseSpec()
		spec.Disks = []machine.DiskAttachment{rw("d")}
		require.NoError(t, svc.Create(ctx, "m", request("m"), spec))
		require.NoError(t, svc.Start(ctx, "m"))
		require.False(t, e.guest.DiskFound("m", "d"), "setup: the guest has no record of the disk")

		requireKind(t, svc.DetachDisk(ctx, "m", "d"), machine.ErrPreconditionFailed)
		assert.Zero(t, e.driver.Calls("detach"))

		require.NoError(t, svc.Stop(ctx, "m", time.Second))
		require.NoError(t, svc.DetachDisk(ctx, "m", "d"))
	})

	t.Run("a Driver that cannot detach it after the release has the guest find it again", func(t *testing.T) {
		e := running(t)
		e.driver.FailAt("detach", errBoom)

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		assert.Equal(t, 2, e.guest.Calls("find-disk"), "once at the attach and once after the failed detach")
		assert.True(t, e.guest.DiskFound("m", "d"))
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
	})

	t.Run("a Detach retried after the Driver failed and the guest could not find the disk again converges", func(t *testing.T) {
		e := running(t)
		e.driver.FailAt("detach", errBoom)
		e.guest.FailAt("find-disk", errBoom)

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		assert.False(t, e.guest.DiskFound("m", "d"), "the guest has let go and could not take the disk back")
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))

		// Control: the same retry while the Driver still fails stays a
		// failure and not a refusal, so the Driver was asked again.
		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		assert.Equal(t, 2, e.driver.Calls("detach"))

		e.driver.Heal("detach")
		require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))
		assert.Equal(t, 3, e.guest.Calls("release-disk"))
		assert.Equal(t, 3, e.driver.Calls("detach"))
		assert.Empty(t, e.driver.HotDisks("m"))
		assert.Empty(t, attached(t, e, "m"))
	})

	t.Run("a disk found again after a release is released again", func(t *testing.T) {
		e := running(t)
		e.driver.FailAt("detach", errBoom)
		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		require.True(t, e.guest.DiskFound("m", "d"), "the guest took it back")

		e.guest.HoldDisk("m", "d", true)
		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrPreconditionFailed)
		e.guest.HoldDisk("m", "d", false)
		e.driver.Heal("detach")
		require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))
	})
}

// Row "Set resources", column Needs.
func TestSetResources_Needs(t *testing.T) {
	ctx := context.Background()
	target := machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}
	smaller := machine.Resources{VCPUs: 1, MemoryBytes: 512 << 20}

	t.Run("resize capability on a Running or Paused Machine", func(t *testing.T) {
		for _, state := range []machine.State{machine.StateRunning, machine.StatePaused} {
			e := newEnv()
			e.inState(t, "m", state)
			requireKind(t, e.svc.SetResources(ctx, "m", smaller, machine.Resources{}), machine.ErrUnsupported)
			assert.Equal(t, baseSpec().Resources, e.requireState(t, "m", state).Resources)
			assert.Zero(t, e.driver.Calls("resize"))

			with := newEnv(machine.CapabilityResize)
			with.inState(t, "m", state)
			if state == machine.StateRunning {
				require.NoError(t, with.svc.SetResources(ctx, "m", smaller, machine.Resources{}))
			} else {
				// Paused: with the capability it is the guest that cannot answer,
				// a refusal of another kind than the capability's.
				requireKind(t, with.svc.SetResources(ctx, "m", smaller, machine.Resources{}), machine.ErrPreconditionFailed)
			}
		}
	})

	t.Run("a Created or Stopped Machine needs no capability", func(t *testing.T) {
		for _, state := range []machine.State{machine.StateCreated, machine.StateStopped} {
			e := newEnv()
			e.inState(t, "m", state)
			require.NoError(t, e.svc.SetResources(ctx, "m", target, target))
		}
	})

	t.Run("an absolute target with CPU and memory", func(t *testing.T) {
		e := newEnv(machine.CapabilityResize)
		e.created(t, "m")
		requireKind(t, e.svc.SetResources(ctx, "m", machine.Resources{MemoryBytes: 1}, machine.Resources{}), machine.ErrInvalid)
		requireKind(t, e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 1}, machine.Resources{}), machine.ErrInvalid)
		requireKind(t, e.svc.SetResources(ctx, "absent", target, machine.Resources{}), machine.ErrNotFound)
		assert.Equal(t, baseSpec().Resources, e.requireState(t, "m", machine.StateCreated).Resources)
	})
}

// Row "Set resources", column On success.
func TestSetResources_OnSuccess(t *testing.T) {
	ctx := context.Background()
	target := machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}

	t.Run("a Running Machine is resized by the Driver", func(t *testing.T) {
		e := newEnv(machine.CapabilityResize)
		e.ceiling = target
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.SetResources(ctx, "m", target, machine.Resources{}))
		assert.Equal(t, target, e.requireState(t, "m", machine.StateRunning).Resources)
		assert.Equal(t, target, e.driver.Resized("m"))
		rec, _ := e.store.Record("m")
		assert.Equal(t, target, rec.Machine.Resources)
	})

	t.Run("a Created Machine holds the target for its next boot", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		require.NoError(t, e.svc.SetResources(ctx, "m", target, target))
		require.NoError(t, e.svc.Start(ctx, "m"))
		assert.Equal(t, target, e.driver.BootSpec("m").Resources)
		assert.Zero(t, e.driver.Calls("resize"))
	})
}

// Row "Set resources", column Repeat.
func TestSetResources_Repeat(t *testing.T) {
	ctx := context.Background()
	target := machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}
	e := newEnv(machine.CapabilityResize)
	e.ceiling = target
	e.inState(t, "m", machine.StateRunning)
	require.NoError(t, e.svc.SetResources(ctx, "m", target, machine.Resources{}))

	require.NoError(t, e.svc.SetResources(ctx, "m", target, machine.Resources{}))
	assert.Equal(t, 1, e.driver.Calls("resize"), "the Driver was not asked again")
	assert.Equal(t, target, e.requireState(t, "m", machine.StateRunning).Resources)

	// A repeat of the Machine's current value needs no capability either.
	plain := newEnv()
	plain.inState(t, "m", machine.StateRunning)
	require.NoError(t, plain.svc.SetResources(ctx, "m", baseSpec().Resources, machine.Resources{}))
}

// Row "Set resources", column State after failure.
func TestSetResources_StateAfterFailure(t *testing.T) {
	ctx := context.Background()
	target := machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}

	t.Run("the Driver fails and inspect shows the prior value", func(t *testing.T) {
		e := newEnv(machine.CapabilityResize)
		e.ceiling = target
		e.inState(t, "m", machine.StateRunning)
		e.driver.FailAt("resize", errBoom)

		requireKind(t, e.svc.SetResources(ctx, "m", target, machine.Resources{}), machine.ErrFailed)
		assert.Equal(t, baseSpec().Resources, e.requireState(t, "m", machine.StateRunning).Resources)
		rec, _ := e.store.Record("m")
		assert.Equal(t, baseSpec().Resources, rec.Machine.Resources)
	})

	t.Run("the target cannot be recorded on a Machine without a VMM", func(t *testing.T) {
		e := newEnv()
		e.created(t, "m")
		e.store.FailAt("save", errBoom)

		requireKind(t, e.svc.SetResources(ctx, "m", target, target), machine.ErrFailed)
		assert.Equal(t, baseSpec().Resources, e.requireState(t, "m", machine.StateCreated).Resources)
	})
}
