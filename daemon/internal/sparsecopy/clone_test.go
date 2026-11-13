package sparsecopy_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

var errBoom = errors.New("boom")

// duplicated is what one DuplicateWith call reported and the two paths it ran
// over.
type duplicated struct {
	cloned       bool
	err          error
	source, dest string
}

// duplicate runs DuplicateWith over a sparse source and a new destination.
func duplicate(t *testing.T, clone sparsecopy.Cloner) duplicated {
	t.Helper()
	dir := t.TempDir()
	d := duplicated{source: filepath.Join(dir, "source"), dest: filepath.Join(dir, "dest")}
	sparseFile(t, d.source)
	src, err := os.Open(d.source)
	require.NoError(t, err)
	defer src.Close()
	dst, err := os.OpenFile(d.dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	defer dst.Close()
	d.cloned, d.err = sparsecopy.DuplicateWith(clone, dst, src)
	return d
}

// A file system that clones is cloned on, and nothing is copied; the control
// is a file system that cannot, where the same call copies the bytes.
func TestDuplicate_ClonesWhereTheFileSystemCanAndCopiesWhereItCannot(t *testing.T) {
	var cloneCalls int
	clones := func(dst, _ *os.File) error {
		cloneCalls++
		_, err := dst.WriteString("cloned")
		return err
	}
	d := duplicate(t, clones)
	require.NoError(t, d.err)
	assert.True(t, d.cloned, "the clone was taken")
	assert.Equal(t, 1, cloneCalls)
	got, err := os.ReadFile(d.dest)
	require.NoError(t, err)
	assert.Equal(t, "cloned", string(got), "what the clone made is what the call left, with no copy over it")

	d = duplicate(t, func(*os.File, *os.File) error { return sparsecopy.ErrCloneUnsupported })
	require.NoError(t, d.err)
	assert.False(t, d.cloned, "no clone was made")
	requireSameBytes(t, d.source, d.dest)
	assert.Less(t, allocated(t, d.dest), int64(size/8), "the copy is sparse")
}

// A clone that fails for any reason but being unsupported fails the call: a
// copy would hide a failing disk. The control is the unsupported error, which
// the same call answers with a copy.
func TestDuplicate_AFailureOtherThanUnsupportedIsNotCopiedOver(t *testing.T) {
	d := duplicate(t, func(*os.File, *os.File) error { return errBoom })
	require.ErrorIs(t, d.err, errBoom)
	info, err := os.Stat(d.dest)
	require.NoError(t, err)
	assert.Zero(t, info.Size(), "nothing was copied")

	d = duplicate(t, func(*os.File, *os.File) error { return sparsecopy.ErrCloneUnsupported })
	require.NoError(t, d.err)
	requireSameBytes(t, d.source, d.dest)
}

// On the file system the test runs on, Duplicate copies the same bytes by
// whichever way the file system allows, and clones exactly where Clone works:
// on an XFS with reflink=1 or a btrfs the clone path runs.
func TestDuplicate_OnThisFileSystemCopiesTheBytesAndClonesWhereCloneWorks(t *testing.T) {
	dir := t.TempDir()
	source, dest := filepath.Join(dir, "source"), filepath.Join(dir, "dest")
	sparseFile(t, source)
	src, err := os.Open(source)
	require.NoError(t, err)
	defer src.Close()
	dst, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	defer dst.Close()

	probe, err := os.CreateTemp(dir, "probe")
	require.NoError(t, err)
	defer probe.Close()
	probeErr := sparsecopy.Clone(probe, src)
	require.True(t, probeErr == nil || errors.Is(probeErr, sparsecopy.ErrCloneUnsupported), "the probe failed for another reason: %v", probeErr)

	cloned, err := sparsecopy.Duplicate(dst, src)
	require.NoError(t, err)
	assert.Equal(t, probeErr == nil, cloned, "Duplicate clones exactly where Clone works")
	requireSameBytes(t, source, dest)
	t.Logf("reflink on this file system: %v", cloned)
}
