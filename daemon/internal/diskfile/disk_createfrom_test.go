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

// sourceFile writes a file of size bytes whose first bytes are data.
func sourceFile(t *testing.T, size int64, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	_, err = f.WriteAt([]byte(data), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return path
}

func TestCreateFrom_CopiesTheSourceGrowsItAndRecordsWhereItCameFrom(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	source := sourceFile(t, 8<<20, "payload")

	entry := machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 12 << 20, Filesystem: machine.FilesystemNone, FromSnapshot: "snap"}
	require.NoError(t, d.CreateFrom(ctx, entry, source))

	got, err := d.Get(ctx, "d")
	require.NoError(t, err)
	assert.Equal(t, entry, got, "the request, the filesystem, the snapshot and the grown size are what the disk records")
	content, err := os.ReadFile(filepath.Join(dir, "d"))
	require.NoError(t, err)
	assert.Equal(t, "payload", string(content[:7]))
	assert.Equal(t, int64(12<<20), sizeOf(t, filepath.Join(dir, "d")))

	// A write to the new disk does not reach the source.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "d"), []byte("changed"), 0))
	src, err := os.ReadFile(source)
	require.NoError(t, err)
	assert.Equal(t, "payload", string(src[:7]))

	// The control: a disk made by Create is made from no snapshot.
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "plain", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}))
	plain, err := d.Get(ctx, "plain")
	require.NoError(t, err)
	assert.Empty(t, plain.FromSnapshot)
}

func TestCreateFrom_ASizeBelowTheSourceIsInvalidAndAnIdentifierInUseExists(t *testing.T) {
	d, dir := newDisk(t)
	ctx := context.Background()
	source := sourceFile(t, 8<<20, "payload")

	err := d.CreateFrom(ctx, machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 4 << 20, Filesystem: machine.FilesystemNone}, source)
	require.ErrorIs(t, err, machine.ErrDiskInvalid)
	assert.NoFileExists(t, filepath.Join(dir, "d"))
	assert.Empty(t, tempFiles(t, dir), "nothing is left")

	require.NoError(t, d.CreateFrom(ctx, machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}, source), "control: the source's own size is accepted")
	err = d.CreateFrom(ctx, machine.DiskEntry{ID: "d", Request: "other", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}, source)
	require.ErrorIs(t, err, machine.ErrDiskExists)
	got, err := d.Get(ctx, "d")
	require.NoError(t, err)
	assert.Equal(t, machine.RequestID("r"), got.Request, "the first disk is kept")
	assert.Empty(t, tempFiles(t, dir), "nothing is left")
}

func TestCreateFrom_AMissingSourceFailsAndLeavesNothing(t *testing.T) {
	d, dir := newDisk(t)
	err := d.CreateFrom(context.Background(), machine.DiskEntry{ID: "d", Request: "r", SizeBytes: 8 << 20, Filesystem: machine.FilesystemNone}, filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.NotErrorIs(t, err, machine.ErrDiskInvalid)
	assert.Empty(t, entriesOf(t, dir))
}

func tempFiles(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range entriesOf(t, dir) {
		if len(n) >= 4 && n[:4] == "tmp-" {
			out = append(out, n)
		}
	}
	return out
}

func entriesOf(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	require.NoError(t, err)
	var out []string
	for _, e := range es {
		out = append(out, e.Name())
	}
	return out
}
