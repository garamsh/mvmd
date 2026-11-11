package machine_test

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func diskIDs(infos []machine.DiskInfo) []machine.DiskID {
	ids := make([]machine.DiskID, 0, len(infos))
	for _, d := range infos {
		ids = append(ids, d.ID)
	}
	return ids
}

// Row "Create disk", columns Needs and On success.
func TestCreateDisk_MakesADiskTheNodeHolds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)

	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req-1", 1<<20))

	size, held := e.disks.Holds("data")
	assert.True(t, held)
	assert.Equal(t, uint64(1<<20), size)
}

func TestCreateDisk_RejectsWhatNamesNoDisk(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)

	requireKind(t, e.svc.CreateDisk(ctx, "", "req", 1<<20), machine.ErrInvalid)
	requireKind(t, e.svc.CreateDisk(ctx, "data", "", 1<<20), machine.ErrInvalid)
	requireKind(t, e.svc.CreateDisk(ctx, "data", "req", 0), machine.ErrInvalid)

	_, held := e.disks.Holds("data")
	assert.False(t, held)
}

// Row "Create disk", column Repeat.
func TestCreateDisk_RepeatIsIdempotent(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req-1", 1<<20))

	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req-1", 1<<20))
	assert.Equal(t, 1, e.disks.Calls("create"), "the repeat makes nothing")

	requireKind(t, e.svc.CreateDisk(ctx, "data", "req-2", 1<<20), machine.ErrConflict)
	requireKind(t, e.svc.CreateDisk(ctx, "data", "req-1", 2<<20), machine.ErrConflict)
	assert.Equal(t, 1, e.disks.Calls("create"))
}

func TestCreateDisk_RefusesTheRootDiskOfAMachine(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.created(t, "m")

	requireKind(t, e.svc.CreateDisk(ctx, "m-root", "req", 1<<20), machine.ErrConflict)
}

// Row "Create disk", column State after failure.
func TestCreateDisk_StateAfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("a failed create is Failed and a retry makes the disk", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.FailAt("create", errBoom)
		requireKind(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20), machine.ErrFailed)
		_, held := e.disks.Holds("data")
		assert.False(t, held)

		e.disks.Heal("create")
		require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))
	})

	t.Run("a failed lookup is Failed and makes nothing", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.FailAt("get", errBoom)
		requireKind(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20), machine.ErrFailed)
		assert.Zero(t, e.disks.Calls("create"))
	})
}

// Row "List disks", column On success.
func TestListDisks_ReportsEveryDiskWithItsHolder(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.created(t, "m")
	require.NoError(t, e.svc.CreateDisk(ctx, "b-data", "r1", 2<<20))
	require.NoError(t, e.svc.CreateDisk(ctx, "a-data", "r2", 1<<20))
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("b-data")))

	got, err := e.svc.ListDisks(ctx)

	require.NoError(t, err)
	require.Len(t, got, 3, "root disks are listed with the rest")
	assert.Equal(t, machine.DiskInfo{ID: "a-data", SizeBytes: 1 << 20}, got[0])
	assert.Equal(t, machine.DiskInfo{ID: "b-data", SizeBytes: 2 << 20, Holder: "m"}, got[1])
	assert.Equal(t, machine.DiskID("m-root"), got[2].ID)
	assert.Equal(t, machine.MachineID("m"), got[2].Holder)
}

func TestListDisks_ANodeWithNoDiskListsNone(t *testing.T) {
	got, err := newEnv(machine.CapabilityDiskHotplug).svc.ListDisks(context.Background())

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListDisks_FailureIsFailed(t *testing.T) {
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.FailAt("list", errBoom)

	_, err := e.svc.ListDisks(context.Background())

	requireKind(t, err, machine.ErrFailed)
}

// Row "Delete disk", column Needs.
func TestDeleteDisk_RefusesADiskThatHasAHolder(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.created(t, "m")
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("data")))

	requireKind(t, e.svc.DeleteDisk(ctx, "data"), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.DeleteDisk(ctx, "m-root"), machine.ErrPreconditionFailed)
	_, held := e.disks.Holds("data")
	assert.True(t, held)
	_, held = e.disks.Holds("m-root")
	assert.True(t, held)

	// Control: once the holder lets go, the same call removes the disk.
	require.NoError(t, e.svc.DetachDisk(ctx, "m", "data"))
	require.NoError(t, e.svc.DeleteDisk(ctx, "data"))
	_, held = e.disks.Holds("data")
	assert.False(t, held)
}

// Row "Delete disk", column Repeat.
func TestDeleteDisk_AnAbsentDiskSucceeds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))
	require.NoError(t, e.svc.DeleteDisk(ctx, "data"))

	require.NoError(t, e.svc.DeleteDisk(ctx, "data"))
	require.NoError(t, e.svc.DeleteDisk(ctx, "never-was"))
	requireKind(t, e.svc.DeleteDisk(ctx, ""), machine.ErrInvalid)
}

// Row "Delete disk", column State after failure.
func TestDeleteDisk_FailedRemovalLeavesTheDiskUnheldAndDeletable(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.created(t, "m")
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))
	e.disks.FailAt("remove", errBoom)

	requireKind(t, e.svc.DeleteDisk(ctx, "data"), machine.ErrFailed)

	// The failed call took no holder: the disk attaches, detaches and deletes.
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("data")))
	require.NoError(t, e.svc.DetachDisk(ctx, "m", "data"))
	e.disks.Heal("remove")
	require.NoError(t, e.svc.DeleteDisk(ctx, "data"))
	_, held := e.disks.Holds("data")
	assert.False(t, held)
}

func TestDeleteDisk_NoAttachHoldsADiskWhileItIsRemoved(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.created(t, "m")
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))
	entered, release := e.disks.Hold("remove")
	done := make(chan error, 1)
	go func() { done <- e.svc.DeleteDisk(ctx, "data") }()
	<-entered

	requireKind(t, e.svc.AttachDisk(ctx, "m", rw("data")), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.DeleteDisk(ctx, "data"), machine.ErrPreconditionFailed)
	release()

	require.NoError(t, <-done)
	requireKind(t, e.svc.AttachDisk(ctx, "m", rw("data")), machine.ErrNotFound)
}

// Row "Attach disk": a disk Create disk made attaches like any disk the node
// holds, and holds one holder at a time.
func TestAttachDisk_ADiskCreateDiskMade(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.created(t, "m")
	e.created(t, "other")
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))

	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("data")))

	assert.Equal(t, []machine.DiskAttachment{rw("data")}, attached(t, e, "m"))
	requireKind(t, e.svc.AttachDisk(ctx, "other", ro("data")), machine.ErrPreconditionFailed)
	got, err := e.svc.ListDisks(ctx)
	require.NoError(t, err)
	assert.Contains(t, diskIDs(got), machine.DiskID("data"))
}

// ADR 0036 §5: a Machine's root disk has its Machine as holder from before
// its file is published, so Delete disk cannot remove it while Create Machine
// is making it.
func TestDeleteDisk_RefusesARootDiskWhileCreateIsPublishingIt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	entered, release := e.disks.Hold("create-root")
	created := make(chan error, 1)
	go func() { created <- e.svc.Create(ctx, "m", request("m"), baseSpec()) }()
	<-entered

	requireKind(t, e.svc.DeleteDisk(ctx, "m-root"), machine.ErrPreconditionFailed)
	release()

	require.NoError(t, <-created)
	_, held := e.disks.Holds("m-root")
	assert.True(t, held, "the Machine was created with its root disk present")
	requireKind(t, e.svc.DeleteDisk(ctx, "m-root"), machine.ErrPreconditionFailed)
}

func TestDeleteDisk_ARootDiskNameIsFreeAfterAFailedCreate(t *testing.T) {
	ctx := context.Background()
	e := newEnv(machine.CapabilityDiskHotplug)
	e.disks.FailAt("create-root", errBoom)
	requireKind(t, e.svc.Create(ctx, "m", request("m"), baseSpec()), machine.ErrFailed)

	require.NoError(t, e.svc.CreateDisk(ctx, "m-root", "req", 1<<20))
	require.NoError(t, e.svc.DeleteDisk(ctx, "m-root"), "nothing holds the name any longer")
}

func TestDeleteDisk_RefusesARootDiskWhileRestoreIsPublishingIt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	entered, release := e.disks.Hold("create-root-from")
	restored := make(chan error, 1)
	go func() { restored <- e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", machine.NetworkPolicy{}) }()
	<-entered

	requireKind(t, e.svc.DeleteDisk(ctx, "new-root"), machine.ErrPreconditionFailed)
	release()

	require.NoError(t, <-restored)
	_, held := e.disks.Holds("new-root")
	assert.True(t, held, "the Machine was restored with its root disk present")
}

// guard is how long a concurrency test waits for a step that must happen
// before it fails instead of hanging.
const guard = 10 * time.Second

// createDiskInBackground starts a Create disk call and returns its outcome.
func createDiskInBackground(e *env, disk machine.DiskID, request machine.RequestID) <-chan error {
	done := make(chan error, 1)
	go func() { done <- e.svc.CreateDisk(context.Background(), disk, request, 1<<20) }()
	return done
}

func awaitOutcome(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(guard):
		require.FailNow(t, "the call did not end")
		return nil
	}
}

// heldCreate starts a Create disk call and returns once it is making the disk,
// with the release that lets it finish.
func heldCreate(t *testing.T, e *env, disk machine.DiskID, request machine.RequestID) (done <-chan error, release func()) {
	t.Helper()
	entered, release := e.disks.Hold("create")
	done = createDiskInBackground(e, disk, request)
	select {
	case <-entered:
	case <-time.After(guard):
		require.FailNow(t, "the first call did not reach the create")
	}
	return done, release
}

// awaitWaiting returns once n calls hold or wait for the disk's lock.
func awaitWaiting(t *testing.T, e *env, disk machine.DiskID, n int) {
	t.Helper()
	deadline := time.Now().Add(guard)
	for machine.DiskLockUsers(e.svc, disk) < n {
		require.True(t, time.Now().Before(deadline), "the second call never waited for the disk")
		runtime.Gosched()
	}
}

// Runtime API §Calling rules, "A repeat while the first call runs waits".
func TestCreateDisk_ARepeatWhileTheFirstCallRunsWaitsAndMakesNothing(t *testing.T) {
	e := newEnv(machine.CapabilityDiskHotplug)
	first, release := heldCreate(t, e, "data", "req-1")
	second := createDiskInBackground(e, "data", "req-1")
	awaitWaiting(t, e, "data", 2)
	release()

	require.NoError(t, awaitOutcome(t, first))
	require.NoError(t, awaitOutcome(t, second))
	assert.Equal(t, 1, e.disks.Calls("create"), "one call made the disk file")
	size, held := e.disks.Holds("data")
	assert.True(t, held)
	assert.Equal(t, uint64(1<<20), size)
}

func TestCreateDisk_ACallUnderAnotherRequestIDWhileTheFirstRunsIsAConflict(t *testing.T) {
	e := newEnv(machine.CapabilityDiskHotplug)
	first, release := heldCreate(t, e, "data", "req-1")
	second := createDiskInBackground(e, "data", "req-2")
	awaitWaiting(t, e, "data", 2)
	release()

	require.NoError(t, awaitOutcome(t, first))
	requireKind(t, awaitOutcome(t, second), machine.ErrConflict)
	assert.Equal(t, 1, e.disks.Calls("create"), "one call made the disk file")
	assert.Equal(t, 0, machine.DiskLockUsers(e.svc, "data"), "the lock entry is gone once the calls end")
}

func TestCreateDisk_CallsForOtherDisksDoNotWaitForTheOneThatRuns(t *testing.T) {
	e := newEnv(machine.CapabilityDiskHotplug)
	first, release := heldCreate(t, e, "data", "req-1")

	// The first disk's call is still held while the other disk's call ends.
	require.NoError(t, awaitOutcome(t, createDiskInBackground(e, "other", "req-2")))
	_, held := e.disks.Holds("other")
	assert.True(t, held)
	_, held = e.disks.Holds("data")
	assert.False(t, held, "the first call has not finished")

	release()
	require.NoError(t, awaitOutcome(t, first))
}
