package snapshotstore_test

import (
	"os"
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

// How a copy keeps holes is sparsecopy's test; this one is that Put copies
// the root disk through it.
func TestPut_TheCopyOfTheRootDiskIsSparse(t *testing.T) {
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	require.Less(t, allocated(t, files.RootDisk), int64(diskSize/8), "the test's file system does not keep a file sparse, so this test cannot run here")

	require.NoError(t, s.Put(ctx, snapshot("a"), files))
	_, held, err := s.Get(ctx, "a")
	require.NoError(t, err)

	info, err := os.Stat(held.RootDisk)
	require.NoError(t, err)
	assert.Equal(t, int64(diskSize), info.Size())
	assert.Less(t, allocated(t, held.RootDisk), int64(diskSize/8))
	requireWhole(t, s, "a", "a")
}
