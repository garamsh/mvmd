package disksnapshotstore_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/disksnapshotstore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

const size = 8 << 20

var errBoom = errors.New("boom")

// newStore opens a store in a new directory.
func newStore(t *testing.T) (*disksnapshotstore.Store, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "disk-snapshots")
	s, err := disksnapshotstore.New(dir)
	require.NoError(t, err)
	return s, dir
}

// sparseDisk writes a sparse file of size bytes holding data at both ends.
func sparseDisk(t *testing.T, data string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "disk")
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	_, err = f.WriteAt([]byte(data), 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte(data), size-int64(len(data)))
	require.NoError(t, err)
	require.NoError(t, f.Close())
	return path
}

func snap(id string) machine.DiskSnapshot {
	return machine.DiskSnapshot{ID: machine.DiskSnapshotID(id), Disk: "d", Request: "r", SizeBytes: size, Filesystem: machine.FilesystemExt4, CreatedAt: time.Unix(1700000000, 0).UTC()}
}

func allocated(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	return st.Blocks * 512
}

func entries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	require.NoError(t, err)
	var names []string
	for _, e := range es {
		names = append(names, e.Name())
	}
	return names
}

// A snapshot is a copy of the disk that a later write to the disk does not
// reach, and it is sparse.
func TestPut_KeepsASparseCopyTheDiskDoesNotChange(t *testing.T) {
	s, _ := newStore(t)
	disk := sparseDisk(t, "before")
	require.NoError(t, s.Put(context.Background(), snap("s1"), disk))

	f, err := os.OpenFile(disk, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("after!"), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())

	got, path, err := s.Get(context.Background(), "s1")
	require.NoError(t, err)
	assert.Equal(t, snap("s1"), got)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "before", string(content[:6]), "the write after the snapshot did not reach it")
	assert.Less(t, allocated(t, path), int64(size/8), "the snapshot is sparse")
	info, err := os.Stat(path)
	require.NoError(t, err)
	assert.EqualValues(t, size, info.Size())
}

// Put clones where the file system does, and copies where it cannot. The
// control is the other file system.
func TestPut_ClonesWhereTheFileSystemCanAndCopiesWhereItCannot(t *testing.T) {
	var cloned int
	s, _ := newStore(t)
	s.WithClone(func(dst, _ *os.File) error {
		cloned++
		_, err := dst.WriteString("cloned")
		return err
	})
	require.NoError(t, s.Put(context.Background(), snap("s1"), sparseDisk(t, "before")))
	assert.Equal(t, 1, cloned)
	_, path, err := s.Get(context.Background(), "s1")
	require.NoError(t, err)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "cloned", string(content), "what the clone made is the snapshot, with no copy over it")

	s, _ = newStore(t)
	s.WithClone(func(*os.File, *os.File) error { return sparsecopy.ErrCloneUnsupported })
	require.NoError(t, s.Put(context.Background(), snap("s1"), sparseDisk(t, "before")))
	_, path, err = s.Get(context.Background(), "s1")
	require.NoError(t, err)
	content, err = os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "before", string(content[:6]), "with no reflink the bytes are copied")
}

// An identifier names one snapshot: the second Put fails and leaves the first
// and nothing in staging.
func TestPut_AnIdentifierInUseIsRefusedAndTheFirstSnapshotKept(t *testing.T) {
	s, dir := newStore(t)
	require.NoError(t, s.Put(context.Background(), snap("s1"), sparseDisk(t, "first-")))
	err := s.Put(context.Background(), snap("s1"), sparseDisk(t, "second"))
	require.ErrorIs(t, err, machine.ErrDiskSnapshotExists)
	_, path, err := s.Get(context.Background(), "s1")
	require.NoError(t, err)
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	assert.Equal(t, "first-", string(content[:6]))
	assert.Empty(t, entries(t, filepath.Join(dir, "staging")))
}

// A Put that fails at any step leaves no snapshot and nothing in staging.
func TestPut_AFailureAtAnyStepLeavesNothing(t *testing.T) {
	for _, step := range []string{"stage", "copy-disk", "write-record", "publish"} {
		t.Run(step, func(t *testing.T) {
			s, dir := newStore(t)
			s.WithFault(func(at string) error {
				if at == step {
					return errBoom
				}
				return nil
			})
			err := s.Put(context.Background(), snap("s1"), sparseDisk(t, "before"))
			require.ErrorIs(t, err, errBoom)
			_, _, err = s.Get(context.Background(), "s1")
			require.ErrorIs(t, err, machine.ErrDiskSnapshotAbsent)
			assert.Empty(t, entries(t, filepath.Join(dir, "staging")))

			s.WithFault(nil)
			require.NoError(t, s.Put(context.Background(), snap("s1"), sparseDisk(t, "before")), "the control: with no fault the same Put keeps the snapshot")
		})
	}
}

// A Put of a source that is not there fails and leaves nothing.
func TestPut_AMissingSourceFailsAndLeavesNothing(t *testing.T) {
	s, dir := newStore(t)
	err := s.Put(context.Background(), snap("s1"), filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	_, _, gerr := s.Get(context.Background(), "s1")
	require.ErrorIs(t, gerr, machine.ErrDiskSnapshotAbsent)
	assert.Empty(t, entries(t, filepath.Join(dir, "staging")))
}

// An identifier a disk may have, "staging" among them, names a snapshot, and
// List reports each with what it occupies, ordered by identifier.
func TestList_ReportsEachSnapshotWithWhatItOccupies(t *testing.T) {
	s, _ := newStore(t)
	for _, id := range []string{"staging", "b", "a"} {
		require.NoError(t, s.Put(context.Background(), snap(id), sparseDisk(t, "before")))
	}
	all, err := s.List(context.Background())
	require.NoError(t, err)
	require.Len(t, all, 3)
	assert.Equal(t, []machine.DiskSnapshotID{"a", "b", "staging"}, []machine.DiskSnapshotID{all[0].ID, all[1].ID, all[2].ID})
	for _, info := range all {
		assert.Positive(t, info.AllocatedBytes)
		assert.Less(t, info.AllocatedBytes, uint64(size/8), "a sparse snapshot occupies what it holds")
	}
}

// Remove deletes the snapshot and its file, and removing an absent one
// succeeds; a failure after the snapshot stops being visible is reclaimed by
// the next New.
func TestRemove_DeletesTheSnapshotAndAnAbsentOneSucceeds(t *testing.T) {
	s, dir := newStore(t)
	require.NoError(t, s.Put(context.Background(), snap("s1"), sparseDisk(t, "before")))
	require.NoError(t, s.Remove(context.Background(), "s1"))
	_, _, err := s.Get(context.Background(), "s1")
	require.ErrorIs(t, err, machine.ErrDiskSnapshotAbsent)
	require.NoError(t, s.Remove(context.Background(), "s1"), "an absent snapshot is removed")
	assert.Empty(t, entries(t, filepath.Join(dir, "staging")))

	require.NoError(t, s.Put(context.Background(), snap("s2"), sparseDisk(t, "before")))
	s.WithFault(func(at string) error {
		if at == "delete" {
			return errBoom
		}
		return nil
	})
	require.ErrorIs(t, s.Remove(context.Background(), "s2"), errBoom)
	_, _, err = s.Get(context.Background(), "s2")
	require.ErrorIs(t, err, machine.ErrDiskSnapshotAbsent, "the snapshot is gone")
	require.NotEmpty(t, entries(t, filepath.Join(dir, "staging")))
	_, err = disksnapshotstore.New(dir)
	require.NoError(t, err)
	assert.Empty(t, entries(t, filepath.Join(dir, "staging")), "New reclaims what was left")
}

// A Put that crashed leaves a directory in staging that New removes and that
// no Get or List shows.
func TestNew_RemovesWhatACrashedPutLeft(t *testing.T) {
	_, dir := newStore(t)
	left := filepath.Join(dir, "staging", "put-abc")
	require.NoError(t, os.MkdirAll(left, 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(left, "disk"), []byte("x"), 0o600))
	s, err := disksnapshotstore.New(dir)
	require.NoError(t, err)
	assert.Empty(t, entries(t, filepath.Join(dir, "staging")))
	all, err := s.List(context.Background())
	require.NoError(t, err)
	assert.Empty(t, all)
}
