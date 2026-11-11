package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The service records the time of the node's clock in the snapshot it takes,
// and a snapshot taken later carries the later time.
func TestSnapshot_RecordsWhenItWasTakenFromTheClock(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.inState(t, "m", machine.StateRunning)

	require.NoError(t, e.svc.Snapshot(ctx, "m", "first", "r-first"))
	e.clock.Set(epoch.Add(time.Hour))
	require.NoError(t, e.svc.Snapshot(ctx, "m", "second", "r-second"))

	first, _, err := e.snaps.Get(ctx, "first")
	require.NoError(t, err)
	second, _, err := e.snaps.Get(ctx, "second")
	require.NoError(t, err)
	assert.True(t, first.CreatedAt.Equal(epoch), "the clock when it was taken, got %v", first.CreatedAt)
	assert.True(t, second.CreatedAt.Equal(epoch.Add(time.Hour)), "control: a later one carries the later time, got %v", second.CreatedAt)
}

// Row "List images": the size and the time the store reports reach the caller.
func TestListImages_CarriesTheSizeAndTheTimeTheStoreReports(t *testing.T) {
	e := newEnv()
	e.withSpare(t)
	stored := epoch.Add(-time.Hour)
	e.images.SetFacts(spare, 12345, stored)

	got, err := e.svc.ListImages(context.Background())

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, machine.ImageInfo{Digest: image}, got[0], "control: an image the store reports no facts for has none")
	assert.Equal(t, machine.ImageInfo{Digest: spare, SizeBytes: 12345, CreatedAt: stored}, got[1])
}

// Row "List snapshots": the size the store measures reaches the caller beside
// the record.
func TestListSnapshots_CarriesTheSizeTheStoreMeasures(t *testing.T) {
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	e.alsoSnapshotted(t)
	e.snaps.SetSize("snap", 4096)

	got, err := e.svc.ListSnapshots(context.Background())

	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, machine.SnapshotID("other"), got[0].ID)
	assert.Equal(t, uint64(0), got[0].SizeBytes, "control: a snapshot the store measured none for has none")
	assert.Equal(t, machine.SnapshotID("snap"), got[1].ID)
	assert.Equal(t, uint64(4096), got[1].SizeBytes)
	assert.True(t, got[1].CreatedAt.Equal(epoch), "the record's time is listed")
}
