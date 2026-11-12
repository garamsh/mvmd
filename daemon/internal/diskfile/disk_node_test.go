package diskfile_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func newDisk(t *testing.T) (*diskfile.Disk, string) {
	t.Helper()
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)
	return d, dir
}

func TestCreate_MakesAFormattedFileOfTheSizeAndKeepsTheRequest(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()

	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "data", Request: "req-1", SizeBytes: 8 << 20}))

	info, err := os.Stat(filepath.Join(dir, "data"))
	require.NoError(t, err)
	assert.Equal(t, int64(8<<20), info.Size())
	raw, err := os.ReadFile(filepath.Join(dir, "data"))
	require.NoError(t, err)
	assert.Equal(t, []byte{0x53, 0xEF}, raw[1080:1082], "the ext4 superblock magic")

	got, err := d.Get(ctx, "data")
	require.NoError(t, err)
	assert.Equal(t, machine.DiskEntry{ID: "data", Request: "req-1", SizeBytes: 8 << 20, Filesystem: machine.FilesystemExt4}, got)
}

func TestCreate_NeverReplacesADiskThatHasTheIdentifier(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "data", Request: "first", SizeBytes: 8 << 20}))

	err := d.Create(ctx, machine.DiskEntry{ID: "data", Request: "second", SizeBytes: 16 << 20})

	assert.ErrorIs(t, err, machine.ErrDiskExists)
	got, err := d.Get(ctx, "data")
	require.NoError(t, err)
	assert.Equal(t, machine.DiskEntry{ID: "data", Request: "first", SizeBytes: 8 << 20, Filesystem: machine.FilesystemExt4}, got)
	names, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, names, 1, "the refused call left no file behind")
}

func TestCreateRoot_NeverReplacesADiskThatHasTheRootName(t *testing.T) {
	d, _ := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "m-root", Request: "client", SizeBytes: 8 << 20}))

	_, _, err := d.CreateRoot(ctx, owner("m"), 8<<20)

	assert.ErrorIs(t, err, machine.ErrDiskExists)
	got, err := d.Get(ctx, "m-root")
	require.NoError(t, err)
	assert.Equal(t, machine.RequestID("client"), got.Request, "the client's disk is untouched")
}

func TestCreate_RejectsAnIdentifierThatCannotNameAFile(t *testing.T) {
	d, dir := newDisk(t)
	for _, id := range []machine.DiskID{"", ".", "..", "a/b", "tmp-x"} {
		err := d.Create(context.Background(), machine.DiskEntry{ID: id, Request: "r", SizeBytes: 1 << 20})
		assert.ErrorIs(t, err, machine.ErrDiskInvalid, "id %q", id)
	}
	names, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, names)
}

func TestGet_AnAbsentOrNonRegularEntryIsAbsent(t *testing.T) {
	d, dir := newDisk(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a-directory"), 0o700))

	_, err := d.Get(context.Background(), "nothing")
	assert.ErrorIs(t, err, machine.ErrDiskAbsent)
	_, err = d.Get(context.Background(), "a-directory")
	assert.ErrorIs(t, err, machine.ErrDiskAbsent)
}

func TestList_ReportsRegularFilesByIdentifierAndNotTheOnesBeingBuilt(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "b", Request: "r", SizeBytes: 2 << 20}))
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "a", Request: "r", SizeBytes: 1 << 20}))
	_, _, err := d.CreateRoot(ctx, owner("m"), 4<<20)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "tmp-unfinished"), []byte("x"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a-directory"), 0o700))

	got, err := d.List(ctx)

	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, machine.DiskEntry{ID: "a", Request: "r", SizeBytes: 1 << 20, Filesystem: machine.FilesystemExt4}, got[0])
	assert.Equal(t, machine.DiskEntry{ID: "b", Request: "r", SizeBytes: 2 << 20, Filesystem: machine.FilesystemExt4}, got[1])
	assert.Equal(t, machine.DiskEntry{ID: "m-root", SizeBytes: 4 << 20, Filesystem: machine.FilesystemExt4}, got[2], "a root disk has no request")
}

func TestRemove_RemovesACreatedDiskAndAnAbsentOneSucceeds(t *testing.T) {
	d, _ := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "data", Request: "r", SizeBytes: 1 << 20}))

	require.NoError(t, d.Remove(ctx, "data"))
	require.NoError(t, d.Remove(ctx, "data"))

	_, err := d.Get(ctx, "data")
	assert.ErrorIs(t, err, machine.ErrDiskAbsent)
}

func TestStat_ReportsTheSizeAndTheModificationTimeOfTheFile(t *testing.T) {
	d, dir := newDisk(t)
	path := filepath.Join(dir, "data")
	require.NoError(t, os.WriteFile(path, make([]byte, 100), 0o600))
	first := time.Unix(1700000000, 0)
	require.NoError(t, os.Chtimes(path, first, first))

	got, err := d.Stat(context.Background(), "data")
	require.NoError(t, err)
	assert.Equal(t, uint64(100), got.SizeBytes)
	assert.True(t, got.ModTime.Equal(first))

	later := first.Add(time.Minute)
	require.NoError(t, os.Chtimes(path, later, later))
	got, err = d.Stat(context.Background(), "data")
	require.NoError(t, err)
	assert.True(t, got.ModTime.Equal(later), "a later write shows")
}

func TestStat_AnAbsentOrNonRegularEntryIsAbsent(t *testing.T) {
	d, dir := newDisk(t)
	require.NoError(t, os.Mkdir(filepath.Join(dir, "a-directory"), 0o700))

	_, err := d.Stat(context.Background(), "nothing")
	assert.ErrorIs(t, err, machine.ErrDiskAbsent)
	_, err = d.Stat(context.Background(), "a-directory")
	assert.ErrorIs(t, err, machine.ErrDiskAbsent)
	_, err = d.Stat(context.Background(), "../escape")
	assert.Error(t, err)
}
