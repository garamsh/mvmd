package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// requireOwnedBy reads back who owns the disk file: the Machine's VMM user, or
// mvmd when the Machine is empty.
func (e *env) requireOwnedBy(t *testing.T, disk machine.DiskID, id machine.MachineID) {
	t.Helper()
	got, owned := e.disks.Owner(disk)
	if id == "" {
		assert.False(t, owned, "disk %q is owned by %+v, not by mvmd", disk, got)
		return
	}
	m, err := e.svc.Inspect(context.Background(), id)
	require.NoError(t, err)
	require.True(t, owned, "disk %q is owned by mvmd, not by Machine %q", disk, id)
	assert.Equal(t, machine.DiskOwner{ID: id, UID: m.User.UID, GID: m.User.GID}, got)
}

// ADR 0038: a disk file belongs to whoever holds it, so the VMM's user can open it.
func TestDiskFileOwner(t *testing.T) {
	ctx := context.Background()

	t.Run("an attach gives the file to the Machine whatever its state", func(t *testing.T) {
		for _, state := range []machine.State{machine.StateCreated, machine.StateRunning} {
			e := newEnv(machine.CapabilityDiskHotplug)
			e.disks.Provide("d")
			e.inState(t, "m", state)
			e.requireOwnedBy(t, "d", "") // setup: the file starts as mvmd's

			require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
			e.requireOwnedBy(t, "d", "m")
		}
	})

	t.Run("the file is the Machine's before the Driver is asked to open it", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.driver.FailAt("attach", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		assert.Equal(t, 1, e.driver.Calls("attach"), "the Driver was asked")
		assert.Equal(t, 1, e.disks.Calls("give"), "the file was given before it was asked")
	})

	t.Run("a detach gives the file back to mvmd", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.requireOwnedBy(t, "d", "m")

		require.NoError(t, e.svc.DetachDisk(ctx, "m", "d"))
		e.requireOwnedBy(t, "d", "")

		// Control: another Machine takes the file as its own.
		e.created(t, "other")
		require.NoError(t, e.svc.AttachDisk(ctx, "other", rw("d")))
		e.requireOwnedBy(t, "d", "other")
	})

	t.Run("a detach that could not give the file back still releases the holder", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.disks.FailAt("reclaim", errBoom)

		requireKind(t, e.svc.DetachDisk(ctx, "m", "d"), machine.ErrFailed)
		assert.Empty(t, attached(t, e, "m"))
		e.disks.Heal("reclaim")
		e.created(t, "other")
		require.NoError(t, e.svc.AttachDisk(ctx, "other", rw("d")), "the holder was released")
	})

	t.Run("a failed attach gives the file back", func(t *testing.T) {
		failures := []struct {
			name string
			fail func(*env)
		}{
			{"the Driver cannot attach", func(e *env) { e.driver.FailAt("attach", errBoom) }},
			{"the guest cannot find the disk", func(e *env) { e.guest.FailAt("find-disk", errBoom) }},
			{"the record cannot be written", func(e *env) { e.store.FailAt("save", errBoom) }},
		}
		for _, tt := range failures {
			t.Run(tt.name, func(t *testing.T) {
				e := newEnv(machine.CapabilityDiskHotplug)
				e.disks.Provide("d")
				e.inState(t, "m", machine.StateRunning)
				tt.fail(e)

				requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
				assert.Equal(t, 1, e.disks.Calls("give"), "setup: the file was given before the failure")
				e.requireOwnedBy(t, "d", "")
				assert.Empty(t, attached(t, e, "m"))
			})
		}
	})

	t.Run("an attach whose file cannot be given fails and holds nothing", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		e.disks.FailAt("give", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		assert.Zero(t, e.driver.Calls("attach"), "the Driver was not asked")
		assert.Empty(t, attached(t, e, "m"))

		// Control: the holder slot is free and the same call is accepted.
		e.disks.Heal("give")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.requireOwnedBy(t, "d", "m")
	})

	t.Run("Delete Machine gives its attached disks back and keeps their files", func(t *testing.T) {
		e := newEnv()
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateStopped)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.requireOwnedBy(t, "d", "m")

		require.NoError(t, e.svc.Delete(ctx, "m"))
		e.requireOwnedBy(t, "d", "")
		_, held := e.disks.Holds("d")
		assert.True(t, held, "the disk outlives its Machine")
	})

	t.Run("Delete Machine that cannot give a disk back fails and keeps the Machine", func(t *testing.T) {
		e := newEnv()
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateStopped)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		e.disks.FailAt("reclaim", errBoom)

		requireKind(t, e.svc.Delete(ctx, "m"), machine.ErrFailed)
		e.requireState(t, "m", machine.StateStopped)

		e.disks.Heal("reclaim")
		require.NoError(t, e.svc.Delete(ctx, "m"))
		e.requireOwnedBy(t, "d", "")
	})

	t.Run("Create gives the disks it names to the Machine", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("a")
		e.disks.Provide("b")
		spec := baseSpec()
		spec.Disks = []machine.DiskAttachment{rw("a"), ro("b")}
		e.requireOwnedBy(t, "a", "") // setup

		require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec))
		e.requireOwnedBy(t, "a", "m")
		e.requireOwnedBy(t, "b", "m")
	})

	t.Run("a Create that fails gives the disks back", func(t *testing.T) {
		failures := []struct {
			name string
			fail func(*env)
		}{
			{"the file cannot be given", func(e *env) { e.disks.FailAfter("give", 1, errBoom) }},
			{"the record cannot be written", func(e *env) { e.store.FailAt("save", errBoom) }},
		}
		for _, tt := range failures {
			t.Run(tt.name, func(t *testing.T) {
				e := newEnv(machine.CapabilityDiskHotplug)
				e.disks.Provide("a")
				e.disks.Provide("b")
				spec := baseSpec()
				spec.Disks = []machine.DiskAttachment{rw("a"), rw("b")}
				tt.fail(e)

				requireKind(t, e.svc.Create(ctx, "m", request("m"), spec), machine.ErrFailed)
				assert.NotZero(t, e.disks.Calls("give"), "setup: the files were given before the failure")
				e.requireOwnedBy(t, "a", "")
				e.requireOwnedBy(t, "b", "")
				e.requireNoMachine(t, "m")
			})
		}
	})

	t.Run("a restart gives a recorded disk to its Machine again", func(t *testing.T) {
		e := newEnv()
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateStopped)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.disks.Reclaim(ctx, "d")) // a death left the file as mvmd's
		e.requireOwnedBy(t, "d", "")

		svc := e.reopened(t)
		m, err := svc.Inspect(ctx, "m")
		require.NoError(t, err)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, m.Disks)
		e.requireOwnedBy(t, "d", "m")
	})

	t.Run("a restart does not give a disk its Machine again once the record drops it", func(t *testing.T) {
		e := attachedRunning(t)
		require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
		require.NoError(t, e.driver.DetachDisk(ctx, "m", "d"))

		e.reopened(t)
		e.requireOwnedBy(t, "d", "") // the record dropped the disk, so mvmd has it again
	})

	t.Run("a Stop leaves the file with its holder", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
		e.requireOwnedBy(t, "d", "m")
	})
}
