package boltstore_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func execEntries() []machine.ExecEntry {
	return []machine.ExecEntry{
		{
			Machine: "m", ID: "a", Request: "r1", Answered: true,
			Spec:   machine.ExecSpec{Command: []string{"echo", "hi"}, Env: map[string]string{"K": "v"}, WorkingDir: "/tmp"},
			Record: machine.ExecRecord{ID: "a", Status: machine.ExecExited, ExitCode: 3, TermSignal: 9},
		},
		{
			Machine: "m", ID: "b", Request: "r2",
			Spec: machine.ExecSpec{Command: []string{"sleep", "1"}},
		},
		{
			Machine: "m", ID: "c", Request: "r3", Answered: true,
			Spec:   machine.ExecSpec{Command: []string{"true"}},
			Record: machine.ExecRecord{ID: "c", Status: machine.ExecCutShort, Reason: "the command ended with its Machine"},
		},
	}
}

func open(t *testing.T, dir string) *boltstore.DB {
	t.Helper()
	db, err := boltstore.Open(dir)
	require.NoError(t, err)
	return db
}

// Exec entries are read back, whole, after the database is closed and
// reopened, from a Save that carried some and from SaveExec.
func TestStore_ExecEntriesSurviveCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)
	entries := execEntries()
	r := records()[0]
	r.Execs = entries[:2]
	require.NoError(t, db.Machines().Save(ctx, r))
	require.NoError(t, db.Machines().SaveExec(ctx, entries[2]))
	require.NoError(t, db.Close())

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, entries, got[0].Execs, "ordered by exec identifier")
}

// A Save that carries no entries keeps the Machine's, and one that carries
// some replaces only those it names.
func TestStore_SaveKeepsTheExecEntriesItDoesNotName(t *testing.T) {
	ctx := context.Background()
	db := open(t, t.TempDir())
	defer db.Close()
	entries := execEntries()
	r := records()[0]
	r.Execs = entries
	require.NoError(t, db.Machines().Save(ctx, r))

	r.Execs = nil
	r.Machine.State = machine.StateRunning
	require.NoError(t, db.Machines().Save(ctx, r))
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Equal(t, machine.StateRunning, got[0].Machine.State, "the setup: the second Save landed")
	assert.Equal(t, entries, got[0].Execs, "kept")

	changed := entries[1]
	changed.Answered = true
	changed.Record = machine.ExecRecord{ID: "b", Status: machine.ExecCutShort, Reason: "x"}
	r.Execs = []machine.ExecEntry{changed}
	require.NoError(t, db.Machines().Save(ctx, r))
	got, err = db.Machines().Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.ExecEntry{entries[0], changed, entries[2]}, got[0].Execs)
}

// Remove deletes the Machine's entries with its record and leaves another
// Machine's.
func TestStore_RemoveDeletesTheExecEntriesOfTheMachine(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)
	r := records()[0]
	r.Execs = execEntries()
	require.NoError(t, db.Machines().Save(ctx, r))
	other := machine.ExecEntry{Machine: "n", ID: "a", Request: "r9", Spec: machine.ExecSpec{Command: []string{"true"}}}
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "n", Machine: r.Machine, Execs: []machine.ExecEntry{other}}))

	require.NoError(t, db.Machines().Remove(ctx, "m"))
	require.NoError(t, db.Close())

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, []machine.ExecEntry{other}, got[0].Execs, "another Machine's entry is kept")

	// A Machine saved again under the identifier starts with none.
	require.NoError(t, db.Machines().Save(ctx, records()[0]))
	got, err = db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Empty(t, got[0].Execs, "the removed Machine's entries did not come back")
}

// An entry is kept only for a Machine that has a record.
func TestStore_AnExecEntryNeedsItsMachinesRecord(t *testing.T) {
	ctx := context.Background()
	db := open(t, t.TempDir())
	defer db.Close()
	entry := execEntries()[0]

	require.Error(t, db.Machines().SaveExec(ctx, entry))
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)

	// Control: with the record, the same entry is kept.
	require.NoError(t, db.Machines().Save(ctx, records()[0]))
	require.NoError(t, db.Machines().SaveExec(ctx, entry))
	got, err = db.Machines().Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, []machine.ExecEntry{entry}, got[0].Execs)
}

// A database made before exec entries existed has no bucket for them. It
// opens, and what it holds reads as before, with no entries.
func TestStore_ADatabaseFromBeforeExecEntriesStillReads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)
	require.NoError(t, db.Machines().Save(ctx, records()[0]))
	require.NoError(t, db.Close())

	raw, err := bolt.Open(filepath.Join(dir, "mvmd.db"), 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, raw.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket([]byte("execs")) }))
	require.NoError(t, raw.View(func(tx *bolt.Tx) error {
		require.Nil(t, tx.Bucket([]byte("execs")), "the setup: the bucket is absent, as in an old database")
		return nil
	}))
	require.NoError(t, raw.Close())

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, records()[0], got[0])

	// It takes entries from then on.
	require.NoError(t, db.Machines().SaveExec(ctx, execEntries()[0]))
	got, err = db.Machines().Load(ctx)
	require.NoError(t, err)
	assert.Equal(t, execEntries()[:1], got[0].Execs)
}

// An entry that cannot be decoded is returned marked, with its Machine and
// identifier from the bucket names, and its Machine's record and other entries
// are read as usual.
func TestStore_AnUndecodableExecEntryIsMarkedAndItsMachineStillReads(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := open(t, dir)
	r := records()[0]
	r.Execs = execEntries()[:1]
	require.NoError(t, db.Machines().Save(ctx, r))
	require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "n", Machine: r.Machine}))
	require.NoError(t, db.Close())

	raw, err := bolt.Open(filepath.Join(dir, "mvmd.db"), 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("execs")).Bucket([]byte("m")).Put([]byte("broken"), []byte("{not json"))
	}))
	require.NoError(t, raw.Close())

	db = open(t, dir)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 2)
	assert.Equal(t, machine.MachineID("m"), got[0].ID)
	assert.Empty(t, got[0].Unreadable, "the Machine's record is read")
	assert.NotNil(t, got[0].Machine)
	require.Len(t, got[0].Execs, 2)
	assert.Equal(t, execEntries()[0], got[0].Execs[0], "control: the readable entry is read whole")
	broken := got[0].Execs[1]
	assert.Equal(t, machine.MachineID("m"), broken.Machine)
	assert.Equal(t, machine.ExecID("broken"), broken.ID)
	assert.NotEmpty(t, broken.Unreadable)
	assert.Empty(t, got[1].Unreadable)
}
