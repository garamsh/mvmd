package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var snapshotDiskCaps = []machine.Capability{machine.CapabilitySnapshot, machine.CapabilityDiskHotplug}

// snapshottedWithDisks snapshots Machine "src", which has the writable disk
// "d1" and the read-only disk "d2" attached, as "snap", and deletes it, so the
// disks have no holder: the state a restore starts from. Each disk has been
// written, so its size and modification time are not the zero values.
func snapshottedWithDisks(t *testing.T) *env {
	t.Helper()
	return snapshottedWithDisksIn(t, newEnv(snapshotDiskCaps...))
}

// snapshottedWithDisksIn is snapshottedWithDisks on the env it is given.
func snapshottedWithDisksIn(t *testing.T, e *env) *env {
	t.Helper()
	ctx := context.Background()
	e.disks.Provide("d1")
	e.disks.Provide("d2")
	e.disks.Write("d1", 4096)
	e.disks.Write("d2", 8192)
	e.inState(t, "src", machine.StateRunning)
	require.NoError(t, e.svc.AttachDisk(ctx, "src", rw("d1")))
	require.NoError(t, e.svc.AttachDisk(ctx, "src", ro("d2")))
	require.NoError(t, e.svc.Snapshot(ctx, "src", "snap", "r-snap"))
	require.NoError(t, e.svc.Stop(ctx, "src", time.Second))
	require.NoError(t, e.svc.Delete(ctx, "src"))
	e.requireNoDiskHolder(t, "d1", "d2")
	return e
}

func (e *env) requireNoDiskHolder(t *testing.T, disks ...machine.DiskID) {
	t.Helper()
	listed, err := e.svc.ListDisks(context.Background())
	require.NoError(t, err)
	for _, d := range listed {
		for _, want := range disks {
			if d.ID == want {
				assert.Empty(t, d.Holder, "disk %q has a holder", want)
			}
		}
	}
}

func TestSnapshot_RecordsTheDisksOfTheMachine(t *testing.T) {
	ctx := context.Background()

	for _, from := range []machine.State{machine.StateRunning, machine.StatePaused} {
		t.Run("a "+from.String()+" Machine with disks", func(t *testing.T) {
			e := newEnv(snapshotDiskCaps...)
			e.disks.Provide("d1")
			e.disks.Provide("d2")
			e.disks.Write("d1", 4096)
			e.disks.Write("d2", 8192)
			e.inState(t, "m", machine.StateRunning)
			require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d1")))
			require.NoError(t, e.svc.AttachDisk(ctx, "m", ro("d2")))
			if from == machine.StatePaused {
				require.NoError(t, e.svc.Pause(ctx, "m"))
			}
			d1, err := e.disks.Stat(ctx, "d1")
			require.NoError(t, err)
			d2, err := e.disks.Stat(ctx, "d2")
			require.NoError(t, err)
			require.NotZero(t, d1.SizeBytes, "the setup: the disk was written")

			require.NoError(t, e.svc.Snapshot(ctx, "m", "snap", "r"))

			all, err := e.svc.ListSnapshots(ctx)
			require.NoError(t, err)
			require.Len(t, all, 1)
			assert.Equal(t, []machine.SnapshotDisk{
				{Disk: "d1", SizeBytes: d1.SizeBytes, ModTime: d1.ModTime},
				{Disk: "d2", ReadOnly: true, SizeBytes: d2.SizeBytes, ModTime: d2.ModTime},
			}, all[0].Disks)
			e.requireState(t, "m", from)
		})
	}

	t.Run("a Machine with no disk records none", func(t *testing.T) {
		e := newEnv(snapshotDiskCaps...)
		e.snapshotted(t, "m", machine.StateRunning)
		all, err := e.svc.ListSnapshots(ctx)
		require.NoError(t, err)
		require.Len(t, all, 1)
		assert.Empty(t, all[0].Disks)
	})

	t.Run("a disk whose file cannot be read fails the snapshot and returns the guest to Running", func(t *testing.T) {
		e := newEnv(snapshotDiskCaps...)
		e.disks.Provide("d1")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d1")))
		e.disks.FailAt("stat", errBoom)

		requireKind(t, e.svc.Snapshot(ctx, "m", "snap", "r"), machine.ErrFailed)
		assert.Empty(t, e.snaps.Held())
		e.requireState(t, "m", machine.StateRunning)

		e.disks.Heal("stat")
		require.NoError(t, e.svc.Snapshot(ctx, "m", "snap", "r"))
	})
}

// A restore takes exactly the recorded disks, or is precondition failed naming
// the disk and why, with nothing created (ADR 0041).
func TestRestoreSnapshot_TakesTheRecordedDisks(t *testing.T) {
	ctx := context.Background()

	t.Run("disks that are held by the node, free and unchanged", func(t *testing.T) {
		e := snapshottedWithDisks(t)

		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))

		m := e.requireState(t, "new", machine.StateRunning)
		assert.Equal(t, []machine.DiskAttachment{rw("d1"), ro("d2")}, m.Disks)
		listed, err := e.svc.ListDisks(ctx)
		require.NoError(t, err)
		holders := map[machine.DiskID]machine.MachineID{}
		for _, d := range listed {
			holders[d.ID] = d.Holder
		}
		assert.Equal(t, machine.MachineID("new"), holders["d1"])
		assert.Equal(t, machine.MachineID("new"), holders["d2"])
		e.requireOwnedBy(t, "d1", "new")
		e.requireOwnedBy(t, "d2", "new")
		var devices []machine.DiskID
		for _, dev := range e.driver.RestoredWith("new").Devices {
			devices = append(devices, dev.Disk)
		}
		assert.Equal(t, []machine.DiskID{"", "new-root", "d1", "d2"}, devices, "the Driver is given the recorded disks, which its saved state holds (ADR 0041)")
	})

	for _, tt := range []struct {
		name   string
		break_ func(*env)
		reason string
	}{
		{"absent", func(e *env) { require.NoError(t, e.svc.DeleteDisk(ctx, "d2")) }, "absent"},
		{"held by a Machine", func(e *env) {
			e.created(t, "other")
			require.NoError(t, e.svc.AttachDisk(ctx, "other", rw("d2")))
		}, "held"},
		{"changed in its modification time", func(e *env) { e.disks.Write("d2", 8192) }, "changed"},
		{"changed in its size", func(e *env) { e.disks.Resize("d2", 8193) }, "changed"},
	} {
		t.Run("a disk that is "+tt.name+" refuses the restore and creates nothing", func(t *testing.T) {
			e := snapshottedWithDisks(t)
			tt.break_(e)
			gives, claims := e.disks.Calls("give"), e.net.Calls("allocate")

			err := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)

			requireKind(t, err, machine.ErrPreconditionFailed)
			assert.Contains(t, err.Error(), `"d2"`, "the disk is named")
			assert.Contains(t, err.Error(), tt.reason, "the reason is named")
			e.requireNoMachine(t, "new")
			assert.Equal(t, gives, e.disks.Calls("give"), "no file was given")
			assert.Equal(t, claims, e.net.Calls("allocate"), "nothing was allocated")
			assert.Zero(t, e.disks.Calls("create-root-from"))
			_, owned := e.disks.Owner("d1")
			assert.False(t, owned, "the unchanged disk was not given")
			// The unchanged disk holds no slot: another Machine can take it.
			e.created(t, "free")
			require.NoError(t, e.svc.AttachDisk(ctx, "free", rw("d1")))
		})
	}
}

// A failure after the disks' slots were taken releases them and gives the files
// back.
func TestRestoreSnapshot_AFailureAfterTheDisksWereTakenReleasesThem(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		fail func(*env)
		heal func(*env)
	}{
		{"the Driver cannot load the snapshot", func(e *env) { e.driver.FailAt("restore", errBoom) }, func(e *env) { e.driver.Heal("restore") }},
		{"the second disk cannot be given", func(e *env) { e.disks.FailAfter("give", e.disks.Calls("give")+1, errBoom) }, func(e *env) { e.disks.Heal("give") }},
		{"the root disk cannot be made", func(e *env) { e.disks.FailAt("create-root-from", errBoom) }, func(e *env) { e.disks.Heal("create-root-from") }},
		{"the network cannot be allocated", func(e *env) { e.net.FailAt("allocate", errBoom) }, func(e *env) { e.net.Heal("allocate") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := snapshottedWithDisks(t)
			tt.fail(e)

			requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), machine.ErrFailed)

			e.requireNoMachine(t, "new")
			e.requireNoDiskHolder(t, "d1", "d2")
			_, owned1 := e.disks.Owner("d1")
			_, owned2 := e.disks.Owner("d2")
			assert.False(t, owned1, "the file of d1 is mvmd's again")
			assert.False(t, owned2, "the file of d2 is mvmd's again")

			// Control: the disks are free to be taken, by the restore itself.
			tt.heal(e)
			require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-again", "snap", narrowPolicy))
			assert.Len(t, e.requireState(t, "new", machine.StateRunning).Disks, 2)
		})
	}
}

func TestRestoreSnapshot_RepeatWithDisksReturnsTheOriginalOutcome(t *testing.T) {
	ctx := context.Background()

	t.Run("a restore that succeeded", func(t *testing.T) {
		e := snapshottedWithDisks(t)
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
		gives := e.disks.Calls("give")

		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), "the disks the Machine holds do not refuse its own repeat")
		assert.Equal(t, gives, e.disks.Calls("give"), "nothing was given a second time")
		assert.Len(t, e.requireState(t, "new", machine.StateRunning).Disks, 2)

		// Control: another request ID for the same Machine is a conflict.
		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "another", "snap", narrowPolicy), machine.ErrConflict)
	})

	t.Run("a restore that failed", func(t *testing.T) {
		e := snapshottedWithDisks(t)
		e.driver.FailAt("restore", errBoom)
		first := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)
		requireKind(t, first, machine.ErrFailed)
		gives := e.disks.Calls("give")

		e.driver.Heal("restore")
		assert.Equal(t, first, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), "the original outcome")
		assert.Equal(t, gives, e.disks.Calls("give"), "the repeat took nothing")
		e.requireNoDiskHolder(t, "d1", "d2")

		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-second", "snap", narrowPolicy), "control: a new request ID is a new attempt")
	})
}

// A refusal after the disks' slots were claimed and before any file was given
// releases the slots and leaves the files mvmd's, and a restore after the cause
// is gone succeeds.
func TestRestoreSnapshot_ARefusalAfterTheDisksWereClaimedReleasesThem(t *testing.T) {
	ctx := context.Background()

	requireReleased := func(t *testing.T, e *env) {
		t.Helper()
		_, err := e.svc.Inspect(ctx, "new")
		requireKind(t, err, machine.ErrNotFound)
		e.requireNoDiskHolder(t, "d1", "d2")
		_, owned1 := e.disks.Owner("d1")
		_, owned2 := e.disks.Owner("d2")
		assert.False(t, owned1, "the file of d1 is mvmd's")
		assert.False(t, owned2, "the file of d2 is mvmd's")
	}

	t.Run("the UID range is used up", func(t *testing.T) {
		e := newEnvWithRangeAndCaps(machine.UIDRange{First: 1000, Count: 1}, snapshotDiskCaps...)
		snapshottedWithDisksIn(t, e)
		e.created(t, "other") // takes the one UID

		require.Error(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
		requireReleased(t, e)

		require.NoError(t, e.svc.Delete(ctx, "other"))
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-again", "snap", narrowPolicy), "control: the UID is free")
		assert.Len(t, e.requireState(t, "new", machine.StateRunning).Disks, 2)
	})

	t.Run("the name of the new root disk is held", func(t *testing.T) {
		e := snapshottedWithDisks(t)
		require.NoError(t, e.svc.CreateDisk(ctx, machine.RootDiskID("new"), "r-root", 1<<20))
		e.created(t, "other")
		require.NoError(t, e.svc.AttachDisk(ctx, "other", rw(machine.RootDiskID("new"))))

		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), machine.ErrPreconditionFailed)
		requireReleased(t, e)

		require.NoError(t, e.svc.DetachDisk(ctx, "other", machine.RootDiskID("new")))
		require.NoError(t, e.svc.DeleteDisk(ctx, machine.RootDiskID("new")))
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-again", "snap", narrowPolicy), "control: the name is free")
	})
}
