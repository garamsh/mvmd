package boltstore_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestBackup_CopiesTheRecordsAndRefusesWhileMvmdHoldsTheFileOrWhenTheCopyExists(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := boltstore.Open(dir)
	require.NoError(t, err)
	rec := machine.Record{ID: "m", Request: machine.Request{ID: "r"}}
	require.NoError(t, db.Machines().Save(ctx, rec))

	dest := filepath.Join(t.TempDir(), "copy.db")
	_, err = boltstore.Backup(dir, dest)
	require.Error(t, err, "mvmd holds the file, so no copy is made")
	assert.NoFileExists(t, dest, "and no partial file is left")

	require.NoError(t, db.Close())
	n, err := boltstore.Backup(dir, dest)
	require.NoError(t, err)
	info, err := os.Stat(dest)
	require.NoError(t, err)
	assert.Equal(t, n, info.Size())
	assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())

	restored := t.TempDir()
	require.NoError(t, os.Rename(dest, filepath.Join(restored, "mvmd.db")))
	copyDB, err := boltstore.Open(restored)
	require.NoError(t, err)
	t.Cleanup(func() { _ = copyDB.Close() })
	records, err := copyDB.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, records, 1)
	assert.Equal(t, machine.MachineID("m"), records[0].ID, "the copy holds the record")

	_, err = boltstore.Backup(dir, filepath.Join(restored, "mvmd.db"))
	assert.ErrorContains(t, err, "not overwritten", "control: a copy that exists is not overwritten")
}
