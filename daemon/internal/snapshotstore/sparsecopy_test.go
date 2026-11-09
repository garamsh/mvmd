package snapshotstore_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allocated is the bytes of the disk a file holds blocks for.
func allocated(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

func TestPut_TheCopyOfTheRootDiskIsSparse(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")

	// The setup made what it says: a source that is sparse here, and a plain
	// copy of it that this file system allocates in full. Without the second,
	// a test of the first would pass on a file system that never makes holes.
	source := allocated(t, files.RootDisk)
	require.Less(t, source, int64(diskSize/8), "the test's file system does not keep a file sparse, so this test cannot run here")
	dense := filepath.Join(f.root, "dense")
	in, err := os.Open(files.RootDisk)
	require.NoError(t, err)
	out, err := os.Create(dense)
	require.NoError(t, err)
	_, err = io.Copy(out, in)
	require.NoError(t, err)
	require.NoError(t, out.Close())
	require.NoError(t, in.Close())
	require.GreaterOrEqual(t, allocated(t, dense), int64(diskSize), "a plain copy is not allocated in full here, so this test could not tell a sparse copy")

	require.NoError(t, s.Put(ctx, snapshot("a"), files))
	_, held, err := s.Get(ctx, "a")
	require.NoError(t, err)

	info, err := os.Stat(held.RootDisk)
	require.NoError(t, err)
	assert.Equal(t, int64(diskSize), info.Size())
	assert.Less(t, allocated(t, held.RootDisk), int64(diskSize/8))
	requireWhole(t, s, "a", "a")
}

// A block of zeros that the source has allocated is not allocated in the
// copy, which is what keeps it sparse where the file system tells no holes.
func TestPut_TheCopyDoesNotAllocateBlocksOfZeros(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	// The source is rewritten with zeros written through, so every block is
	// allocated, and with the tag's data in the places the test reads.
	d, err := os.OpenFile(files.RootDisk, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = d.WriteAt(bytes.Repeat([]byte{0}, diskSize), 0)
	require.NoError(t, err)
	for off, b := range disk("a") {
		_, err := d.WriteAt(b, off)
		require.NoError(t, err)
	}
	require.NoError(t, d.Sync())
	require.NoError(t, d.Close())
	require.GreaterOrEqual(t, allocated(t, files.RootDisk), int64(diskSize), "the source is not allocated in full, so this test cannot tell")

	require.NoError(t, s.Put(ctx, snapshot("a"), files))
	_, held, err := s.Get(ctx, "a")
	require.NoError(t, err)

	assert.Less(t, allocated(t, held.RootDisk), int64(diskSize/8))
	requireWhole(t, s, "a", "a")
}

func TestPut_TheCopyOfAnEmptyAndOfAFullDiskIsTheSameBytes(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":        {},
		"all holes":    nil,
		"all data":     bytes.Repeat([]byte("data"), 3*(1<<20)/4+5), // not a multiple of a block
		"one odd byte": {7},
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			s := f.open(t)
			files := f.files(t, "a")
			require.NoError(t, os.Remove(files.RootDisk))
			require.NoError(t, os.WriteFile(files.RootDisk, content, 0o600))
			if name == "all holes" {
				require.NoError(t, os.Truncate(files.RootDisk, 1<<20))
			}
			want, err := os.ReadFile(files.RootDisk)
			require.NoError(t, err)

			require.NoError(t, s.Put(ctx, snapshot("a"), files))
			_, held, err := s.Get(ctx, "a")
			require.NoError(t, err)

			got, err := os.ReadFile(held.RootDisk)
			require.NoError(t, err)
			assert.True(t, bytes.Equal(want, got), "the copy differs from the source")
		})
	}
}
