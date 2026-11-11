package snapshotstore_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/snapshotstore"
)

// records returns the snapshots of the listing, without what List measured.
func records(infos []machine.SnapshotInfo) []machine.Snapshot {
	out := make([]machine.Snapshot, 0, len(infos))
	for _, info := range infos {
		out = append(out, info.Snapshot)
	}
	return out
}

func TestList_ReturnsEverySnapshotOrderedByIdentifier(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)

	none, err := s.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, none, "a store that holds none lists none, and staging is not a snapshot")

	// Put in an order that is neither the identifiers' nor the hashes' of them.
	for _, id := range []string{"b", "c", "a"} {
		require.NoError(t, s.Put(ctx, snapshot(id), f.files(t, id)))
	}

	got, err := s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.Snapshot{snapshot("a"), snapshot("b"), snapshot("c")}, records(got), "every record, whole")

	require.NoError(t, s.Remove(ctx, "b"))
	got, err = s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.Snapshot{snapshot("a"), snapshot("c")}, records(got), "a removed snapshot is no longer listed")
}

func TestList_ASnapshotThatCannotBeReadFailsTheListing(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	require.NoError(t, s.Put(ctx, snapshot("b"), f.files(t, "b")))
	_, files, err := s.Get(ctx, "a")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(filepath.Dir(files.RootDisk), "record.json"), []byte("{not json"), 0o600))

	_, err = s.List(ctx)

	assert.Error(t, err)
	got, _, err := s.Get(ctx, "b") // control: the other snapshot is as it was
	require.NoError(t, err)
	assert.Equal(t, machine.SnapshotID("b"), got.ID)
}

func TestList_EndedContextAnswersNothing(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("a"), f.files(t, "a")))
	ended, cancel := context.WithCancel(ctx)
	cancel()

	_, err := s.List(ended)
	assert.ErrorIs(t, err, context.Canceled)
	_, err = s.List(ctx) // control
	assert.NoError(t, err)
}

// sizeOf returns the size List reports for the snapshot.
func sizeOf(t *testing.T, s *snapshotstore.Store, id machine.SnapshotID) uint64 {
	t.Helper()
	all, err := s.List(ctx)
	require.NoError(t, err)
	for _, info := range all {
		if info.ID == id {
			return info.SizeBytes
		}
	}
	require.Failf(t, "not listed", "snapshot %q", id)
	return 0
}

// The size is the allocated blocks of every file of the snapshot's directory:
// a sparse root disk counts what it holds, not its length, and a state file
// counts.
func TestList_ReportsTheBlocksEveryFileOfTheSnapshotOccupies(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	const stateBytes = 1 << 20

	require.NoError(t, s.Put(ctx, snapshot("sparse"), f.files(t, "sparse")))
	sparse := sizeOf(t, s, "sparse")
	assert.GreaterOrEqual(t, sparse, uint64(3*512), "the three chunks the root disk holds")
	assert.Less(t, sparse, uint64(diskSize/2), "the holes of the root disk are not counted")

	// Control: the same disk written whole is as large as its length.
	dense := f.files(t, "dense")
	require.NoError(t, os.WriteFile(dense.RootDisk, bytes.Repeat([]byte{0xab}, diskSize), 0o600))
	require.NoError(t, s.Put(ctx, snapshot("dense"), dense))
	assert.GreaterOrEqual(t, sizeOf(t, s, "dense"), uint64(diskSize), "the size is the blocks and not a constant")

	// A state file is counted.
	big := f.files(t, "big")
	require.NoError(t, os.WriteFile(big.State[0], bytes.Repeat([]byte{0xcd}, stateBytes), 0o600))
	require.NoError(t, s.Put(ctx, snapshot("big"), big))
	assert.GreaterOrEqual(t, sizeOf(t, s, "big"), uint64(stateBytes), "the state file is in the size, which the rest of it alone does not reach")
}

// A record written before the store recorded when it stored a snapshot reads
// back with no time, through a restart; one written now keeps its time.
func TestList_ARecordWithoutACreationTimeReadsBackWithNone(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	require.NoError(t, s.Put(ctx, snapshot("old"), f.files(t, "old")))
	require.NoError(t, s.Put(ctx, snapshot("kept"), f.files(t, "kept")))

	_, files, err := s.Get(ctx, "old")
	require.NoError(t, err)
	path := filepath.Join(filepath.Dir(files.RootDisk), "record.json")
	var fields map[string]any
	require.NoError(t, json.Unmarshal([]byte(read(t, path)), &fields))
	require.Contains(t, fields, "created_unix_nano", "the record the store writes now holds the time")
	delete(fields, "created_unix_nano")
	legacy, err := json.Marshal(fields)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, legacy, 0o600))

	all, err := f.open(t).List(ctx)

	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, machine.SnapshotID("kept"), all[0].ID)
	assert.True(t, all[0].CreatedAt.Equal(snapshot("kept").CreatedAt), "control: a record written now keeps its time")
	assert.Equal(t, machine.SnapshotID("old"), all[1].ID)
	assert.True(t, all[1].CreatedAt.IsZero(), "an earlier record has no time")
	assert.Greater(t, all[1].SizeBytes, uint64(0), "its size is measured all the same")
}
