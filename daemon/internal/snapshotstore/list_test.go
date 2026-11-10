package snapshotstore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

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
	assert.Equal(t, []machine.Snapshot{snapshot("a"), snapshot("b"), snapshot("c")}, got, "every record, whole")

	require.NoError(t, s.Remove(ctx, "b"))
	got, err = s.List(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.Snapshot{snapshot("a"), snapshot("c")}, got, "a removed snapshot is no longer listed")
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
