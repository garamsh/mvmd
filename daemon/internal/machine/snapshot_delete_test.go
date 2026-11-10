package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// alsoSnapshotted adds a second snapshot "other" of the Running Machine src,
// so that a call on "snap" has a snapshot beside it that it must leave alone.
func (e *env) alsoSnapshotted(t *testing.T) {
	t.Helper()
	require.NoError(t, e.svc.Snapshot(context.Background(), "src", "other", "r-other"))
	require.Equal(t, []machine.SnapshotID{"snap", "other"}, e.snaps.Held(), "the setup: both snapshots exist")
}

// Row "List snapshots", column On success.
func TestListSnapshots_ReportsWhatTheNodeHolds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)

	empty, err := e.svc.ListSnapshots(ctx)
	require.NoError(t, err)
	assert.Empty(t, empty, "a node that holds none lists none")

	e.snapshotted(t, "src", machine.StateRunning)
	e.alsoSnapshotted(t)

	got, err := e.svc.ListSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, machine.SnapshotID("other"), got[0].ID, "ordered by identifier")
	assert.Equal(t, machine.SnapshotID("snap"), got[1].ID)
	assert.Equal(t, machine.MachineID("src"), got[1].Machine, "the source Machine")
	assert.Equal(t, e.driver.Info().Name, got[1].Compat.Driver)
	assert.NotEmpty(t, got[1].Spec.Image, "the image the snapshot's Machine runs")
}

// Row "List snapshots", column After failure.
func TestListSnapshots_StoreFailure(t *testing.T) {
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	e.snaps.FailAt("list", errBoom)

	_, err := e.svc.ListSnapshots(context.Background())

	requireKind(t, err, machine.ErrFailed)
	assert.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held(), "a read changes nothing")
}

// Row "Delete snapshot", columns Needs, On success and Repeat.
func TestDeleteSnapshot_RemovesTheSnapshotAndRepeatSucceeds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	e.alsoSnapshotted(t)

	require.NoError(t, e.svc.DeleteSnapshot(ctx, "snap"))
	assert.Equal(t, []machine.SnapshotID{"other"}, e.snaps.Held(), "only the named snapshot is gone")

	require.NoError(t, e.svc.DeleteSnapshot(ctx, "snap"), "a snapshot that is already gone is deleted")
	require.NoError(t, e.svc.DeleteSnapshot(ctx, "never-held"), "so is one the node never held")
	assert.Equal(t, []machine.SnapshotID{"other"}, e.snaps.Held())

	requireKind(t, e.svc.DeleteSnapshot(ctx, ""), machine.ErrInvalid)
}

// A snapshot is a copy, so the Machine it was taken from does not hold it.
func TestDeleteSnapshot_DoesNotRefuseASnapshotWhoseMachineExists(t *testing.T) {
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)

	require.NoError(t, e.svc.DeleteSnapshot(context.Background(), "snap"))

	assert.Empty(t, e.snaps.Held())
	e.requireState(t, "src", machine.StateRunning)
}

// Row "Delete snapshot", column Needs: no call is using the snapshot.
func TestDeleteSnapshot_RefusesASnapshotACallIsUsing(t *testing.T) {
	ctx := context.Background()

	t.Run("a restore is reading it", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		e.alsoSnapshotted(t)
		entered, release := e.driver.Hold("restore")
		restoring := make(chan error, 1)
		go func() { restoring <- e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy) }()
		<-entered

		// The near miss: another snapshot is not in use, and goes.
		require.NoError(t, e.svc.DeleteSnapshot(ctx, "other"))
		err := e.svc.DeleteSnapshot(ctx, "snap")
		held := e.snaps.Held()
		release()

		requireKind(t, err, machine.ErrPreconditionFailed)
		assert.Equal(t, []machine.SnapshotID{"snap"}, held, "the refusal changed nothing, and the other snapshot was deleted")
		require.NoError(t, <-restoring)
		e.requireState(t, "new", machine.StateRunning)

		require.NoError(t, e.svc.DeleteSnapshot(ctx, "snap"), "once the restore has returned, nothing uses the snapshot")
		assert.Empty(t, e.snaps.Held())
		e.requireState(t, "new", machine.StateRunning)
	})

	t.Run("a Snapshot call is still writing it", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "src", machine.StateRunning)
		require.NoError(t, e.svc.Snapshot(ctx, "src", "other", "r-other"))
		entered, release := e.driver.Hold("resume")
		snapshotting := make(chan error, 1)
		go func() { snapshotting <- e.svc.Snapshot(ctx, "src", "snap", "r-snap") }()
		<-entered
		require.Equal(t, []machine.SnapshotID{"other", "snap"}, e.snaps.Held(), "the setup: the store holds it while the guest is still paused")

		require.NoError(t, e.svc.DeleteSnapshot(ctx, "other"), "the near miss: another snapshot goes")
		err := e.svc.DeleteSnapshot(ctx, "snap")
		held := e.snaps.Held()
		release()

		requireKind(t, err, machine.ErrPreconditionFailed)
		assert.Equal(t, []machine.SnapshotID{"snap"}, held)
		require.NoError(t, <-snapshotting)
		require.NoError(t, e.svc.DeleteSnapshot(ctx, "snap"))
		assert.Empty(t, e.snaps.Held())
	})
}

// Row "Delete snapshot", column After failure.
func TestDeleteSnapshot_StoreFailure(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	e.snaps.FailAt("remove", errBoom)

	requireKind(t, e.svc.DeleteSnapshot(ctx, "snap"), machine.ErrFailed)

	e.snaps.Heal("remove")
	require.NoError(t, e.svc.DeleteSnapshot(ctx, "snap"), "a repeat after the failure deletes it")
	assert.Empty(t, e.snaps.Held())
}
