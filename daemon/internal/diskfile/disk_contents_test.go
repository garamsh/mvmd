package diskfile_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestCreateWithContents_WritesAtOffsetZeroAndRecordsTheDigest(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	contents := []byte("#cloud-config")

	require.NoError(t, d.CreateWithContents(ctx, machine.DiskEntry{ID: "seed", Request: "r", SizeBytes: 1024, Filesystem: machine.FilesystemNone, ContentsDigest: machine.ContentsDigest(contents)}, contents))

	raw, err := os.ReadFile(filepath.Join(dir, "seed"))
	require.NoError(t, err)
	assert.Equal(t, contents, raw[:len(contents)])
	assert.Len(t, raw, 1024)
	got, err := d.Get(ctx, "seed")
	require.NoError(t, err)
	assert.Equal(t, machine.ContentsDigest(contents), got.ContentsDigest)
	plain := machine.DiskEntry{ID: "plain", Request: "r", SizeBytes: 1024, Filesystem: machine.FilesystemNone}
	require.NoError(t, d.Create(ctx, plain))
	got, err = d.Get(ctx, "plain")
	require.NoError(t, err)
	assert.Empty(t, got.ContentsDigest, "control: a disk made with none records none")
}

func TestCreateWithContents_RefusesWhatDoesNotFitOrIsFormattedAndLeavesNoFile(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()

	err := d.CreateWithContents(ctx, machine.DiskEntry{ID: "long", Request: "r", SizeBytes: 4, Filesystem: machine.FilesystemNone}, []byte("12345"))
	assert.ErrorIs(t, err, machine.ErrDiskInvalid)
	err = d.CreateWithContents(ctx, machine.DiskEntry{ID: "fmt", Request: "r", SizeBytes: 1 << 20, Filesystem: machine.FilesystemExt4}, []byte("1"))
	assert.ErrorIs(t, err, machine.ErrDiskInvalid)

	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries)
	require.NoError(t, d.CreateWithContents(ctx, machine.DiskEntry{ID: "fits", Request: "r", SizeBytes: 5, Filesystem: machine.FilesystemNone}, []byte("12345")), "control: the same bytes in a size that holds them")
}
