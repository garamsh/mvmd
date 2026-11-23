package catalog_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/catalog"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

func snapshotNode(t *testing.T) (*node, *memory.DiskSnapshotStore) {
	t.Helper()
	n := newNode(t)
	store := memory.NewDiskSnapshotStore()
	n.catalog = catalog.NewService(n.images, n.snapshots, n.store, catalog.WithDiskSnapshots(store))
	require.NoError(t, store.Put(context.Background(), machine.DiskSnapshot{ID: "s", Disk: "d", Request: "r"}, "/disks/d"))
	return n, store
}

func TestDiskSnapshots_AreListedAndAnAbsentOneIsDeletedWithSuccess(t *testing.T) {
	ctx := context.Background()
	n, store := snapshotNode(t)

	all, err := n.catalog.ListDiskSnapshots(ctx)
	require.NoError(t, err)
	require.Len(t, all, 1)
	assert.Equal(t, machine.DiskSnapshotID("s"), all[0].ID)

	require.NoError(t, n.catalog.DeleteDiskSnapshot(ctx, "s"))
	assert.False(t, store.Holds("s"))
	require.NoError(t, n.catalog.DeleteDiskSnapshot(ctx, "s"), "an absent snapshot succeeds")
	assert.ErrorIs(t, kind(t, n.catalog.DeleteDiskSnapshot(ctx, "a/b")), machine.ErrInvalid)
}

func TestDeleteDiskSnapshot_IsRefusedWhileALeaseIsHeldOnTheSnapshot(t *testing.T) {
	ctx := context.Background()
	for name, exclusive := range map[string]bool{"a call that writes it": true, "a call that reads it": false} {
		t.Run(name, func(t *testing.T) {
			n, store := snapshotNode(t)
			release := n.catalog.HoldDiskSnapshot("s", exclusive)

			assert.ErrorIs(t, kind(t, n.catalog.DeleteDiskSnapshot(ctx, "s")), machine.ErrPreconditionFailed)
			assert.True(t, store.Holds("s"), "the refused delete removed nothing")
			release()

			require.NoError(t, n.catalog.DeleteDiskSnapshot(ctx, "s"), "control: once the call has ended the snapshot goes")
		})
	}
}

func TestDiskSnapshots_ANodeWithNoStoreIsUnsupported(t *testing.T) {
	ctx := context.Background()
	n := newNode(t)

	_, err := n.catalog.ListDiskSnapshots(ctx)
	assert.ErrorIs(t, kind(t, err), machine.ErrUnsupported)
	assert.ErrorIs(t, kind(t, n.catalog.DeleteDiskSnapshot(ctx, "s")), machine.ErrUnsupported)
}
