package diskfile_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestCreate_ANoneDiskIsNotFormattedAndReportsIt(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()

	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "raw", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}))
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "fs", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemExt4}))

	raw, err := os.ReadFile(filepath.Join(dir, "raw"))
	require.NoError(t, err)
	assert.Equal(t, []byte{0, 0}, raw[1080:1082], "no superblock was written")
	got, err := d.Get(ctx, "raw")
	require.NoError(t, err)
	assert.Equal(t, machine.FilesystemNone, got.Filesystem)

	// Control: the ext4 disk of the same setup has the magic and reports ext4.
	formatted, err := os.ReadFile(filepath.Join(dir, "fs"))
	require.NoError(t, err)
	assert.Equal(t, []byte{0x53, 0xEF}, formatted[1080:1082])
	got, err = d.Get(ctx, "fs")
	require.NoError(t, err)
	assert.Equal(t, machine.FilesystemExt4, got.Filesystem)

	all, err := d.List(ctx)
	require.NoError(t, err)
	require.Len(t, all, 2)
	assert.Equal(t, machine.FilesystemExt4, all[0].Filesystem)
	assert.Equal(t, machine.FilesystemNone, all[1].Filesystem)
}

func TestGet_ADiskWithNoFilesystemRecordedIsExt4(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "old", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}))
	got, err := d.Get(ctx, "old")
	require.NoError(t, err)
	require.Equal(t, machine.FilesystemNone, got.Filesystem, "the setup: it was recorded")
	require.NoError(t, unix.Removexattr(filepath.Join(dir, "old"), "user.mvmd.filesystem"))

	got, err = d.Get(ctx, "old")

	require.NoError(t, err)
	assert.Equal(t, machine.FilesystemExt4, got.Filesystem)
}

func TestCreate_AFilesystemItDoesNotKnowLeavesNoDisk(t *testing.T) {
	d, dir := newDisk(t)

	err := d.Create(context.Background(), machine.DiskEntry{ID: "x", Request: "r", SizeBytes: 1 << 20, Filesystem: "xfs"})

	require.ErrorIs(t, err, machine.ErrDiskInvalid)
	left, rerr := os.ReadDir(dir)
	require.NoError(t, rerr)
	assert.Empty(t, left)
}
