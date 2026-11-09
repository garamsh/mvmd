package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A repeated attach of a disk the record lists on a Running Machine is not
// answered from the record, which a death may have left ahead of the VMM and
// the guest (ADR 0023).
func TestAttachDisk_ARepeatConvergesOnARunningMachine(t *testing.T) {
	ctx := context.Background()
	// halfDone is a Running Machine whose record lists "d" that the VMM and the
	// guest lack: a death after the record.
	halfDone := func(t *testing.T) *env {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
		require.NoError(t, e.driver.DetachDisk(ctx, "m", "d"))
		return e
	}

	t.Run("a disk the VMM lacks is attached now", func(t *testing.T) {
		e := halfDone(t)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		assert.Equal(t, []machine.DiskID{"d"}, e.driver.HotDisks("m"))
		assert.True(t, e.guest.DiskFound("m", "d"))
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"))
	})

	t.Run("a disk the VMM lacks and cannot be attached is not reported attached", func(t *testing.T) {
		e := halfDone(t)
		e.driver.FailAt("attach", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		assert.Empty(t, attached(t, e, "m"))
		e.created(t, "other")
		require.NoError(t, e.svc.AttachDisk(ctx, "other", rw("d")), "the holder is released")
	})

	t.Run("a disk the guest has not found is found now, or the repeat fails", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.guest.ReleaseDisk(ctx, "m", "d"))
		e.guest.FailAt("find-disk", errBoom)

		requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrFailed)
		assert.Equal(t, []machine.DiskAttachment{rw("d")}, attached(t, e, "m"), "the repeat does not unplug what the guest may use")

		e.guest.Heal("find-disk")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		assert.True(t, e.guest.DiskFound("m", "d"))
	})

	t.Run("a Machine that is not Running is answered from the record", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("d")
		e.created(t, "m")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")))
		assert.Zero(t, e.driver.Calls("drive"))
	})
}
