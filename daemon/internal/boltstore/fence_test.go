package boltstore_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A Machine's fence record is read back after a close and reopen, a delete
// removes it, and it does not disturb the node's deadline record.
func TestFenceStore_RecordsSurviveReopenApartFromTheNodesDeadline(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)
	node := fencing.Record{Expiry: 100, BootID: "b1", Length: time.Hour, Renewal: 7}
	require.NoError(t, db.Deadline().Save(ctx, node))
	fence := fencing.Record{Expiry: 50, BootID: "b1", Length: time.Minute, Renewal: 2}
	require.NoError(t, db.Fences().Save(ctx, "a", fence))
	require.NoError(t, db.Fences().Save(ctx, "b", fencing.Record{}))
	require.NoError(t, db.Close())

	db = open(t, dir)
	defer db.Close()
	got, err := db.Fences().Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, fence, got["a"])
	assert.Contains(t, got, machine.MachineID("b"))
	assert.Len(t, got, 2)
	gotNode, ok, err := db.Deadline().Load(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, node, gotNode)

	require.NoError(t, db.Fences().Delete(ctx, "a"))
	got, err = db.Fences().Load(ctx)
	require.NoError(t, err)
	assert.NotContains(t, got, machine.MachineID("a"))
	assert.Contains(t, got, machine.MachineID("b"))
}
