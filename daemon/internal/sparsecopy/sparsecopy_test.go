package sparsecopy_test

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

const size = 4 << 20

// allocated is the bytes of the disk a file holds blocks for.
func allocated(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info.Sys().(*syscall.Stat_t).Blocks * 512
}

// copyFile runs Copy from the file at from to a new file at to.
func copyFile(t *testing.T, from, to string) {
	t.Helper()
	src, err := os.Open(from)
	require.NoError(t, err)
	defer src.Close()
	dst, err := os.OpenFile(to, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	require.NoError(t, sparsecopy.Copy(dst, src))
	require.NoError(t, dst.Close())
}

// sparseFile writes a file of size with data in its first, middle and last
// bytes and holes between.
func sparseFile(t *testing.T, path string) {
	t.Helper()
	f, err := os.Create(path)
	require.NoError(t, err)
	require.NoError(t, f.Truncate(size))
	for off, b := range map[int64][]byte{0: []byte("head"), size / 2: []byte("middle"), size - 4: []byte("tail")} {
		_, err := f.WriteAt(b, off)
		require.NoError(t, err)
	}
	require.NoError(t, f.Close())
}

func requireSameBytes(t *testing.T, want, got string) {
	t.Helper()
	w, err := os.ReadFile(want)
	require.NoError(t, err)
	g, err := os.ReadFile(got)
	require.NoError(t, err)
	assert.True(t, bytes.Equal(w, g), "the copy differs from the source")
}

func TestCopy_ASparseSourceStaysSparse(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	sparseFile(t, source)

	// The setup made what it says: a source that is sparse here, and a plain
	// copy of it that this file system allocates in full. Without the second,
	// a test of the first would pass on a file system that never makes holes.
	require.Less(t, allocated(t, source), int64(size/8), "the test's file system does not keep a file sparse, so this test cannot run here")
	dense := filepath.Join(dir, "dense")
	in, err := os.Open(source)
	require.NoError(t, err)
	out, err := os.Create(dense)
	require.NoError(t, err)
	_, err = io.Copy(out, in)
	require.NoError(t, err)
	require.NoError(t, out.Close())
	require.NoError(t, in.Close())
	require.GreaterOrEqual(t, allocated(t, dense), int64(size), "a plain copy is not allocated in full here, so this test could not tell a sparse copy")

	copied := filepath.Join(dir, "copy")
	copyFile(t, source, copied)

	info, err := os.Stat(copied)
	require.NoError(t, err)
	assert.Equal(t, int64(size), info.Size())
	assert.Less(t, allocated(t, copied), int64(size/8))
	requireSameBytes(t, source, copied)
}

// A block of zeros that the source has allocated is not allocated in the
// copy, which is what keeps it sparse where the file system tells no holes.
func TestCopy_DoesNotAllocateBlocksOfZeros(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "source")
	// The source is written with zeros through, so every block is allocated,
	// and with data in the place the test reads.
	f, err := os.Create(source)
	require.NoError(t, err)
	_, err = f.WriteAt(bytes.Repeat([]byte{0}, size), 0)
	require.NoError(t, err)
	_, err = f.WriteAt([]byte("middle"), size/2)
	require.NoError(t, err)
	require.NoError(t, f.Sync())
	require.NoError(t, f.Close())
	require.GreaterOrEqual(t, allocated(t, source), int64(size), "the source is not allocated in full, so this test cannot tell")

	copied := filepath.Join(dir, "copy")
	copyFile(t, source, copied)

	assert.Less(t, allocated(t, copied), int64(size/8))
	requireSameBytes(t, source, copied)
}

func TestCopy_OfAnEmptyAndOfAFullFileIsTheSameBytes(t *testing.T) {
	for name, content := range map[string][]byte{
		"empty":        {},
		"all holes":    nil,
		"all data":     bytes.Repeat([]byte("data"), 3*(1<<20)/4+5), // not a multiple of a block
		"one odd byte": {7},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source")
			require.NoError(t, os.WriteFile(source, content, 0o600))
			if name == "all holes" {
				require.NoError(t, os.Truncate(source, 1<<20))
			}

			copied := filepath.Join(dir, "copy")
			copyFile(t, source, copied)

			requireSameBytes(t, source, copied)
		})
	}
}
