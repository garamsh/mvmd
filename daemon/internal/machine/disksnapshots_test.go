package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ADR 0056: a disk is snapshotted, and a new disk is made from the snapshot.
// These tests hold a disk "d" of diskBytes, which a Machine "m" holds in the
// cases that need a holder.

// snapshotOf takes the snapshot "s" of the disk "d".
func snapshotOf(e *env) error {
	return e.svc.SnapshotDisk(context.Background(), "d", "s", "r-s")
}

func (e *env) requireDiskSnapshot(t *testing.T, id machine.DiskSnapshotID) machine.DiskSnapshot {
	t.Helper()
	s, _, err := e.dsnaps.Get(context.Background(), id)
	require.NoError(t, err, "the node holds disk snapshot %q", id)
	return s
}

// A snapshot records the disk, the request, the size and the filesystem of the
// disk it copied, and the path it copied from is the disk's file.
func TestSnapshotDisk_CopiesTheDiskAndRecordsWhatItIs(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemNone))

	require.NoError(t, snapshotOf(e))

	got := e.requireDiskSnapshot(t, "s")
	assert.Equal(t, machine.DiskSnapshot{ID: "s", Disk: "d", Request: "r-s", SizeBytes: diskBytes, Filesystem: machine.FilesystemNone, CreatedAt: epoch}, got)
	source, _ := e.dsnaps.Source("s")
	assert.Equal(t, "/disks/d", source, "the copy is of the disk's file")
	all, err := e.svc.ListDiskSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, machine.DiskID("d"), all[0].Disk)
}

// A repeat under the same request ID and disk copies nothing again; the same
// identifier under another request ID or another disk is a conflict; the
// control is a new identifier, which is made.
func TestSnapshotDisk_ARepeatSucceedsAndAnotherRequestOrDiskIsAConflict(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	require.NoError(t, e.svc.CreateDisk(ctx, "other", "r-o", diskBytes, machine.FilesystemExt4))
	require.NoError(t, snapshotOf(e))

	require.NoError(t, snapshotOf(e), "a repeat succeeds")
	assert.Equal(t, 1, e.dsnaps.Calls("put"), "nothing was copied again")

	requireKind(t, e.svc.SnapshotDisk(ctx, "d", "s", "r-other"), machine.ErrConflict)
	requireKind(t, e.svc.SnapshotDisk(ctx, "other", "s", "r-s"), machine.ErrConflict)
	assert.Equal(t, 1, e.dsnaps.Calls("put"))
	assert.Equal(t, machine.DiskID("d"), e.requireDiskSnapshot(t, "s").Disk, "the first snapshot is kept")

	require.NoError(t, e.svc.SnapshotDisk(ctx, "d", "s2", "r-s2"), "control: a new identifier is made")
	assert.Equal(t, 2, e.dsnaps.Calls("put"))
}

func TestSnapshotDisk_RefusesWhatIsNotACall(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))

	requireKind(t, e.svc.SnapshotDisk(ctx, "d", "s", ""), machine.ErrInvalid)
	requireKind(t, e.svc.SnapshotDisk(ctx, "d", "", "r"), machine.ErrInvalid)
	requireKind(t, e.svc.SnapshotDisk(ctx, "d", "a/b", "r"), machine.ErrInvalid)
	requireKind(t, e.svc.SnapshotDisk(ctx, "d", "tmp-s", "r"), machine.ErrInvalid)
	requireKind(t, e.svc.SnapshotDisk(ctx, "", "s", "r"), machine.ErrInvalid)
	requireKind(t, e.svc.SnapshotDisk(ctx, "absent", "s", "r"), machine.ErrNotFound)
	assert.Zero(t, e.dsnaps.Calls("put"), "no refused call copied anything")

	require.NoError(t, e.svc.SnapshotDisk(ctx, "d", "s", "r"), "control: the same disk and identifier with a valid call")
}

// A disk a Running Machine holds is copied with the Machine paused, and the
// Machine is Running again afterwards, as it was.
func TestSnapshotDisk_ARunningHolderIsPausedForTheCopyAndResumed(t *testing.T) {
	e := newEnv(hotplugOnly...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	pauses, resumes := e.driver.Calls("pause"), e.driver.Calls("resume")

	require.NoError(t, snapshotOf(e))

	e.requireDiskSnapshot(t, "s")
	assert.Equal(t, pauses+1, e.driver.Calls("pause"), "the Machine was paused")
	assert.Equal(t, resumes+1, e.driver.Calls("resume"), "and resumed")
	e.requireState(t, "m", machine.StateRunning)
}

// A Machine that cannot be paused is not copied under: the call fails and no
// snapshot exists.
func TestSnapshotDisk_AFailedPauseFailsTheCallAndCopiesNothing(t *testing.T) {
	e := newEnv(hotplugOnly...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	e.driver.FailAt("pause", errBoom)

	requireKind(t, snapshotOf(e), machine.ErrFailed)

	assert.False(t, e.dsnaps.Holds("s"))
	assert.Zero(t, e.dsnaps.Calls("put"))
	e.requireState(t, "m", machine.StateRunning)
}

// A Machine that cannot return to Running is Faulted with the reason, the
// snapshot is removed, and the call fails. The control is a copy that fails and
// a resume that works: the Machine is Running and no snapshot is kept.
func TestSnapshotDisk_AHolderThatCannotBeResumedIsFaultedAndTheSnapshotRemoved(t *testing.T) {
	e := newEnv(hotplugOnly...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	e.driver.FailAt("resume", errBoom)

	requireKind(t, snapshotOf(e), machine.ErrFailed)

	m := e.requireState(t, "m", machine.StateFaulted)
	assert.Contains(t, m.FailureReason, "could not be returned to Running")
	assert.False(t, e.dsnaps.Holds("s"), "a snapshot taken under a Machine that is now Faulted is not kept")

	e = newEnv(hotplugOnly...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	e.dsnaps.FailAt("put", errBoom)
	requireKind(t, snapshotOf(e), machine.ErrFailed)
	e.requireState(t, "m", machine.StateRunning)
	assert.Equal(t, 1, e.driver.Calls("resume"), "a failed copy still resumes the Machine")
}

// A Paused, Created or Stopped holder's disk is copied as it is, without a
// pause or a resume; a Faulted holder's is refused.
func TestSnapshotDisk_TheHoldersStateDecides(t *testing.T) {
	for _, tc := range []struct {
		state machine.State
		kind  error
	}{
		{machine.StateCreated, nil},
		{machine.StateStopped, nil},
		{machine.StatePaused, nil},
		{machine.StateFaulted, machine.ErrPreconditionFailed},
	} {
		t.Run(tc.state.String(), func(t *testing.T) {
			e := newEnv(hotplugOnly...)
			e.withDisk(t, tc.state, mountedAt("d"))
			pauses, resumes := e.driver.Calls("pause"), e.driver.Calls("resume")

			err := snapshotOf(e)

			assert.Equal(t, pauses, e.driver.Calls("pause"), "a copy as it is pauses nothing")
			assert.Equal(t, resumes, e.driver.Calls("resume"))
			if tc.kind == nil {
				require.NoError(t, err)
				e.requireDiskSnapshot(t, "s")
				e.requireState(t, "m", tc.state)
				return
			}
			requireKind(t, err, tc.kind)
			assert.False(t, e.dsnaps.Holds("s"))
			assert.Zero(t, e.dsnaps.Calls("put"))
		})
	}
}

// A disk no Machine holds is copied, and while it is copied nothing takes it:
// an attach, a delete and a resize are refused, and ListDisks reports no holder.
func TestSnapshotDisk_WhileACopyRunsTheDiskIsTaken(t *testing.T) {
	e := newEnv(hotplugOnly...)
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	e.created(t, "m")
	entered, release := e.dsnaps.Hold("put")
	done := make(chan error, 1)
	go func() { done <- snapshotOf(e) }()
	<-entered

	requireKind(t, e.svc.DeleteDisk(ctx, "d"), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "d"}), machine.ErrPreconditionFailed)
	all, err := e.svc.ListDisks(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	for _, d := range all {
		if d.ID == "d" {
			assert.Empty(t, d.Holder, "the slot the copy takes is not a Machine")
		}
	}
	release()
	require.NoError(t, awaitOutcome(t, done))

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo), "control: once the copy is made the disk resizes")
}

// A disk snapshot is made from a snapshot whose disk is gone: deleting the
// source disk leaves it.
func TestCreateDiskFromSnapshot_MakesTheDiskFromTheSnapshotAndTakesItsFilesystem(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemNone))
	require.NoError(t, snapshotOf(e))
	require.NoError(t, e.svc.DeleteDisk(ctx, "d"))

	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", grownTo, "", "s"))

	size, held := e.disks.Holds("n")
	require.True(t, held)
	assert.EqualValues(t, grownTo, size)
	source, _ := e.disks.Source("n")
	assert.Equal(t, "/disk-snapshots/s", source, "the new disk is a copy of the snapshot's file")
	all, err := e.svc.ListDisks(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, machine.FilesystemNone, all[0].Filesystem, "it takes the filesystem of the snapshot, not ext4")
	assert.True(t, e.dsnaps.Holds("s"), "deleting the source disk leaves its snapshot")
}

// A filesystem the request names that is not the snapshot's, and a size below
// it, are invalid and make nothing; the controls are the snapshot's own
// filesystem and size.
func TestCreateDiskFromSnapshot_AnotherFilesystemOrASmallerSizeIsInvalid(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	require.NoError(t, snapshotOf(e))

	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes, machine.FilesystemNone, "s"), machine.ErrInvalid)
	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes-1, "", "s"), machine.ErrInvalid)
	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes, "ntfs", "s"), machine.ErrInvalid)
	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", 0, "", "s"), machine.ErrInvalid)
	_, held := e.disks.Holds("n")
	assert.False(t, held)
	assert.Zero(t, e.disks.Calls("create-from"))

	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes, machine.FilesystemExt4, "s"), "control: the snapshot's filesystem and size")
	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n2", "r-n2", diskBytes, "", "s"), "control: an unspecified filesystem")
}

func TestCreateDiskFromSnapshot_AnUnknownSnapshotIsNotFound(t *testing.T) {
	e := newEnv()
	ctx := context.Background()

	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes, "", "absent"), machine.ErrNotFound)

	_, held := e.disks.Holds("n")
	assert.False(t, held)
	e.dsnaps.Provide(machine.DiskSnapshot{ID: "absent", Disk: "d", Request: "r", SizeBytes: diskBytes, Filesystem: machine.FilesystemExt4})
	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes, "", "absent"), "control: once the snapshot is held the same call is made")
}

// The snapshot joins the request ID, size and filesystem a repeat is compared
// against: the same call repeats, even once the snapshot is deleted; any other
// is a conflict, and so is a plain Create disk of that identifier.
func TestCreateDiskFromSnapshot_ARepeatSucceedsAndAnythingElseIsAConflict(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	require.NoError(t, snapshotOf(e))
	require.NoError(t, e.svc.SnapshotDisk(ctx, "d", "s2", "r-s2"))
	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", grownTo, "", "s"))

	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", grownTo, "", "s"), "a repeat succeeds")
	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", grownTo, machine.FilesystemExt4, "s"), "naming the snapshot's filesystem is the same call")
	assert.Equal(t, 1, e.disks.Calls("create-from"), "nothing was made again")
	require.NoError(t, e.svc.DeleteDiskSnapshot(ctx, "s"))
	require.NoError(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", grownTo, "", "s"), "a repeat needs no snapshot")

	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-other", grownTo, "", "s"), machine.ErrConflict)
	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", grownTo+1, "", "s"), machine.ErrConflict)
	requireKind(t, e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", grownTo, "", "s2"), machine.ErrConflict)
	requireKind(t, e.svc.CreateDisk(ctx, "n", "r-n", grownTo, machine.FilesystemExt4), machine.ErrConflict)
	assert.Equal(t, 1, e.disks.Calls("create-from"))
}

// Delete disk snapshot removes the snapshot, and an absent one succeeds; the
// disk it was taken of is not touched.
func TestDeleteDiskSnapshot_RemovesTheSnapshotAndAnAbsentOneSucceeds(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	require.NoError(t, snapshotOf(e))

	require.NoError(t, e.svc.DeleteDiskSnapshot(ctx, "s"))
	assert.False(t, e.dsnaps.Holds("s"))
	_, held := e.disks.Holds("d")
	assert.True(t, held, "the disk is left")

	require.NoError(t, e.svc.DeleteDiskSnapshot(ctx, "s"), "an absent snapshot succeeds")
	requireKind(t, e.svc.DeleteDiskSnapshot(ctx, "a/b"), machine.ErrInvalid)
}

// A snapshot a SnapshotDisk call is writing or a Create disk call is reading
// is not deleted: the call is refused and the snapshot is kept; the control is
// the same delete once the call has ended.
func TestDeleteDiskSnapshot_IsRefusedWhileACallIsUsingTheSnapshot(t *testing.T) {
	ctx := context.Background()

	t.Run("a SnapshotDisk call", func(t *testing.T) {
		e := newEnv()
		require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
		entered, release := e.dsnaps.Hold("put")
		done := make(chan error, 1)
		go func() { done <- snapshotOf(e) }()
		<-entered

		requireKind(t, e.svc.DeleteDiskSnapshot(ctx, "s"), machine.ErrPreconditionFailed)
		release()
		require.NoError(t, awaitOutcome(t, done))

		assert.True(t, e.dsnaps.Holds("s"), "the refused delete removed nothing")
		require.NoError(t, e.svc.DeleteDiskSnapshot(ctx, "s"))
	})

	t.Run("a Create disk call", func(t *testing.T) {
		e := newEnv()
		require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
		require.NoError(t, snapshotOf(e))
		entered, release := e.disks.Hold("create-from")
		done := make(chan error, 1)
		go func() { done <- e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes, "", "s") }()
		<-entered

		requireKind(t, e.svc.DeleteDiskSnapshot(ctx, "s"), machine.ErrPreconditionFailed)
		release()
		require.NoError(t, awaitOutcome(t, done))

		assert.True(t, e.dsnaps.Holds("s"))
		require.NoError(t, e.svc.DeleteDiskSnapshot(ctx, "s"))
	})
}

// A Create disk call for a snapshot waits for the SnapshotDisk call that is
// still writing it, and then finds it.
func TestCreateDiskFromSnapshot_WaitsForASnapshotStillBeingWritten(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	entered, release := e.dsnaps.Hold("put")
	snapshot := make(chan error, 1)
	go func() { snapshot <- snapshotOf(e) }()
	<-entered
	create := make(chan error, 1)
	go func() { create <- e.svc.CreateDiskFromSnapshot(ctx, "n", "r-n", diskBytes, "", "s") }()
	select {
	case err := <-create:
		require.FailNow(t, "the call did not wait for the snapshot", "it ended with %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	release()

	require.NoError(t, awaitOutcome(t, snapshot))
	require.NoError(t, awaitOutcome(t, create))
	_, held := e.disks.Holds("n")
	assert.True(t, held)
}

// A node with no store of disk snapshots refuses every call on one.
func TestDiskSnapshots_ANodeWithNoStoreIsUnsupported(t *testing.T) {
	e := newEnv()
	svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events)
	ctx := context.Background()
	require.NoError(t, svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))

	requireKind(t, svc.SnapshotDisk(ctx, "d", "s", "r"), machine.ErrUnsupported)
	requireKind(t, svc.CreateDiskFromSnapshot(ctx, "n", "r", diskBytes, "", "s"), machine.ErrUnsupported)
	_, err := svc.ListDiskSnapshots(ctx)
	requireKind(t, err, machine.ErrUnsupported)
	requireKind(t, svc.DeleteDiskSnapshot(ctx, "s"), machine.ErrUnsupported)
}
