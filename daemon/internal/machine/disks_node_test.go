package machine_test

import (
	"context"
	"testing"

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
	e := newEnv()

	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req-1", 1<<20))

	size, held := e.disks.Holds("data")
	assert.True(t, held)
	assert.Equal(t, uint64(1<<20), size)
}

func TestCreateDisk_RejectsWhatNamesNoDisk(t *testing.T) {
	ctx := context.Background()
	e := newEnv()

	requireKind(t, e.svc.CreateDisk(ctx, "", "req", 1<<20), machine.ErrInvalid)
	requireKind(t, e.svc.CreateDisk(ctx, "data", "", 1<<20), machine.ErrInvalid)
	requireKind(t, e.svc.CreateDisk(ctx, "data", "req", 0), machine.ErrInvalid)

	_, held := e.disks.Holds("data")
	assert.False(t, held)
}

// Row "Create disk", column Repeat.
func TestCreateDisk_RepeatIsIdempotent(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req-1", 1<<20))

	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req-1", 1<<20))
	assert.Equal(t, 1, e.disks.Calls("create"), "the repeat makes nothing")

	requireKind(t, e.svc.CreateDisk(ctx, "data", "req-2", 1<<20), machine.ErrConflict)
	requireKind(t, e.svc.CreateDisk(ctx, "data", "req-1", 2<<20), machine.ErrConflict)
	assert.Equal(t, 1, e.disks.Calls("create"))
}

func TestCreateDisk_RefusesTheRootDiskOfAMachine(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.created(t, "m")

	requireKind(t, e.svc.CreateDisk(ctx, "m-root", "req", 1<<20), machine.ErrConflict)
}

// Row "Create disk", column State after failure.
func TestCreateDisk_StateAfterFailure(t *testing.T) {
	ctx := context.Background()

	t.Run("a failed create is Failed and a retry makes the disk", func(t *testing.T) {
		e := newEnv()
		e.disks.FailAt("create", errBoom)
		requireKind(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20), machine.ErrFailed)
		_, held := e.disks.Holds("data")
		assert.False(t, held)

		e.disks.Heal("create")
		require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))
	})

	t.Run("a failed lookup is Failed and makes nothing", func(t *testing.T) {
		e := newEnv()
		e.disks.FailAt("get", errBoom)
		requireKind(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20), machine.ErrFailed)
		assert.Zero(t, e.disks.Calls("create"))
	})
}

// Row "List disks", column On success.
func TestListDisks_ReportsEveryDiskWithItsHolder(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
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
	got, err := newEnv().svc.ListDisks(context.Background())

	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestListDisks_FailureIsFailed(t *testing.T) {
	e := newEnv()
	e.disks.FailAt("list", errBoom)

	_, err := e.svc.ListDisks(context.Background())

	requireKind(t, err, machine.ErrFailed)
}

// Row "Delete disk", column Needs.
func TestDeleteDisk_RefusesADiskThatHasAHolder(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
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
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "data", "req", 1<<20))
	require.NoError(t, e.svc.DeleteDisk(ctx, "data"))

	require.NoError(t, e.svc.DeleteDisk(ctx, "data"))
	require.NoError(t, e.svc.DeleteDisk(ctx, "never-was"))
	requireKind(t, e.svc.DeleteDisk(ctx, ""), machine.ErrInvalid)
}

// Row "Delete disk", column State after failure.
func TestDeleteDisk_FailedRemovalLeavesTheDiskUnheldAndDeletable(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
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
	e := newEnv()
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
	e := newEnv()
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
