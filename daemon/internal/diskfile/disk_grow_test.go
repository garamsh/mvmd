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

func sizeOf(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Size()
}

func TestGrow_ExtendsTheFileAndKeepsWhatIsInIt(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemExt4}))
	path := filepath.Join(dir, "d")
	before, err := os.ReadFile(path)
	require.NoError(t, err)

	require.NoError(t, d.Grow(ctx, "d", 16<<20))

	assert.Equal(t, int64(16<<20), sizeOf(t, path))
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, before, after[:len(before)], "the filesystem is untouched")
	got, err := d.Get(ctx, "d")
	require.NoError(t, err)
	assert.Equal(t, uint64(16<<20), got.SizeBytes)
	assert.Equal(t, machine.FilesystemExt4, got.Filesystem, "what the disk records survives")
	assert.Equal(t, machine.RequestID("r"), got.Request)
}

func TestGrow_AnEqualSizeChangesNothingAndASmallerOneIsRefused(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}))
	path := filepath.Join(dir, "d")

	require.NoError(t, d.Grow(ctx, "d", 8<<20))
	assert.Equal(t, int64(8<<20), sizeOf(t, path))

	err := d.Grow(ctx, "d", 4<<20)
	require.ErrorIs(t, err, machine.ErrDiskInvalid)
	assert.Equal(t, int64(8<<20), sizeOf(t, path), "a refused call leaves the file")

	require.NoError(t, d.Grow(ctx, "d", 9<<20), "control: a larger size grows")
	assert.Equal(t, int64(9<<20), sizeOf(t, path))
}

func TestGrow_AnAbsentDiskAndAnInvalidIdentifierAreRefused(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()

	require.ErrorIs(t, d.Grow(ctx, "ghost", 1<<20), machine.ErrDiskAbsent)
	require.ErrorIs(t, d.Grow(ctx, "..", 1<<20), machine.ErrDiskInvalid)
	require.ErrorIs(t, d.Grow(ctx, "a/b", 1<<20), machine.ErrDiskInvalid)
	require.ErrorIs(t, d.Grow(ctx, "d", 1<<63), machine.ErrDiskInvalid)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "a refused call makes no file")
}

// A link at the disk's name is not followed: the file it names is not grown.
func TestGrow_ALinkAtTheDisksNameIsNotFollowed(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	outside := filepath.Join(t.TempDir(), "outside")
	require.NoError(t, os.WriteFile(outside, make([]byte, 4096), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(dir, "link")))

	require.ErrorIs(t, d.Grow(ctx, "link", 1<<20), machine.ErrDiskAbsent)
	assert.Equal(t, int64(4096), sizeOf(t, outside), "the file the link names is not grown")

	// Control: the same file at a plain name is grown, so the link was the cause.
	require.NoError(t, os.Rename(outside, filepath.Join(dir, "plain")))
	require.NoError(t, d.Grow(ctx, "plain", 1<<20))
	assert.Equal(t, int64(1<<20), sizeOf(t, filepath.Join(dir, "plain")))
}

func TestGrow_KeepsTheOwnerModeAndAttributesOfTheFile(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}))
	path := filepath.Join(dir, "d")
	require.NoError(t, os.Chmod(path, 0o640))
	var before unix.Stat_t
	require.NoError(t, unix.Stat(path, &before))

	require.NoError(t, d.Grow(ctx, "d", 16<<20))

	var after unix.Stat_t
	require.NoError(t, unix.Stat(path, &after))
	assert.Equal(t, before.Ino, after.Ino, "the same file, not a copy")
	assert.Equal(t, before.Uid, after.Uid)
	assert.Equal(t, before.Mode, after.Mode)
}
