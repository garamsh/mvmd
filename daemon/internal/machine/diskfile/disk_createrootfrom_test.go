package diskfile_test

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/diskfile"
)

const sourceSize = 4 << 20

// sourceDisk writes a sparse file of sourceSize with data at its start, middle
// and end, outside the disks directory, and returns its path.
func sourceDisk(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "source")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(sourceSize))
	for off, b := range map[int64][]byte{0: []byte("head"), sourceSize / 2: []byte("middle"), sourceSize - 4: []byte("tail")} {
		_, err := f.WriteAt(b, off)
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())
	return path
}

func allocated(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

func entries(t *testing.T, dir string) int {
	t.Helper()
	e, err := os.ReadDir(dir)
	require.NoError(t, err)
	return len(e)
}

func TestCreateRootFrom_IsASparseCopyOfTheSource(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)
	source := sourceDisk(t)
	// The setup made what it says: a source that is sparse here, so a copy
	// that allocated it in full could be told from one that did not.
	require.Less(t, allocated(t, source), int64(sourceSize/8), "the test's file system does not keep a file sparse, so this test cannot run here")

	id, err := d.CreateRootFrom(context.Background(), owner("m5"), source)
	require.NoError(t, err)

	assert.Equal(t, machine.DiskID("m5-root"), id)
	path, err := d.Locate(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(dir, "m5-root"), path)
	want, err := os.ReadFile(source)
	require.NoError(t, err)
	got, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(want, got), "the disk differs from the source")
	assert.Less(t, allocated(t, path), int64(sourceSize/8))
	assert.Equal(t, 1, entries(t, dir), "only the root disk file, no temporary one")
}

func TestCreateRootFrom_RefusesASourceThatIsNotARegularFile(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	for name, source := range map[string]string{
		"directory": t.TempDir(),
		"device":    "/dev/null",
		"absent":    filepath.Join(t.TempDir(), "absent"),
	} {
		t.Run(name, func(t *testing.T) {
			_, err := d.CreateRootFrom(context.Background(), owner("m6"), source)
			assert.Error(t, err)
			assert.Equal(t, 0, entries(t, dir), "nothing is left behind")
		})
	}

	// Control: the same call with a regular file is accepted, so the refusals
	// above are of the source and not of the owner or the directory.
	_, err = d.CreateRootFrom(context.Background(), owner("m6"), sourceDisk(t))
	require.NoError(t, err)
	assert.Equal(t, 1, entries(t, dir))
}

func TestCreateRootFrom_RefusesAnUnsafeDiskName(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)
	source := sourceDisk(t)

	for _, id := range []string{"../evil", "has/slash"} {
		_, err := d.CreateRootFrom(context.Background(), owner(id), source)
		assert.Error(t, err, "id %q should be rejected", id)
	}
	assert.Equal(t, 0, entries(t, dir))

	_, err = d.CreateRootFrom(context.Background(), owner("fine"), source)
	require.NoError(t, err, "control: a safe name with the same source is accepted")
}

func TestCreateRootFrom_RefusesACancelledContext(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)
	source := sourceDisk(t)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = d.CreateRootFrom(cancelled, owner("m7"), source)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, entries(t, dir))

	_, err = d.CreateRootFrom(context.Background(), owner("m7"), source)
	require.NoError(t, err, "control: a live context with the same call is accepted")
}

// Only root may give a file to another user, so the failure is only reachable
// without root; with it, the test below reads the ownership instead.
func TestCreateRootFrom_LeavesNothingWhenTheChownFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can chown to any user, so the chown cannot fail here")
	}
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)
	source := sourceDisk(t)
	other := owner("m8")
	other.UID++

	_, err = d.CreateRootFrom(context.Background(), other, source)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "chown")
	assert.Equal(t, 0, entries(t, dir), "the temporary file is removed")

	_, err = d.CreateRootFrom(context.Background(), owner("m8"), source)
	require.NoError(t, err, "control: the caller's own user is accepted")
}

func TestCreateRootFrom_IsOwnedByTheOwner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("giving a file to another user needs root")
	}
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)
	// The directory is the VMM user's to enter, as the node's would be.
	require.NoError(t, os.Chmod(dir, 0o755))

	id, err := d.CreateRootFrom(context.Background(), machine.DiskOwner{ID: "m9", UID: 23456, GID: 34567}, sourceDisk(t))
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dir, string(id)))
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	assert.Equal(t, uint32(23456), st.Uid)
	assert.Equal(t, uint32(34567), st.Gid)
}
