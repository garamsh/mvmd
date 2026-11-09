package supervisor

import (
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func sumOf(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func TestPresent_AFilePinnedByDigestIsOpenedOnlyWhenItHasIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kernel")
	require.NoError(t, os.WriteFile(path, []byte("the pinned kernel"), 0o644))

	f, err := present(path, sumOf([]byte("the pinned kernel")))
	require.NoError(t, err, "control: the file with the pinned digest is presented")
	st, err := f.Stat()
	require.NoError(t, err)
	byPath, err := os.Stat(path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(byPath, st), "what is presented is the file at the path")
	f.Close()

	_, err = present(path, sumOf([]byte("another kernel")))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "digest")
	assert.Contains(t, err.Error(), sumOf([]byte("the pinned kernel")), "the error names what the file has")
}

// The file the hash saw is the file the caller mounts: it is the descriptor
// present returns, and a file put at the path after the hash is another file
// (ADR 0019).
func TestPresent_AFileReplacedAtItsPathAfterTheHashIsNotTheFilePresented(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kernel")
	require.NoError(t, os.WriteFile(path, []byte("the pinned kernel"), 0o644))
	replaced := false
	afterHash = func() {
		other := filepath.Join(dir, "other")
		require.NoError(t, os.WriteFile(other, []byte("a replacement"), 0o644))
		require.NoError(t, os.Rename(other, path))
		replaced = true
	}
	t.Cleanup(func() { afterHash = func() {} })

	f, err := present(path, sumOf([]byte("the pinned kernel")))
	require.NoError(t, err, "the digest is of the file that was opened, whatever the path holds by the end")
	defer f.Close()

	require.True(t, replaced, "setup: the path was replaced between the hash and the end")
	now, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "a replacement", string(now), "setup: the path now holds the replacement")
	st, err := f.Stat()
	require.NoError(t, err)
	byPath, err := os.Stat(path)
	require.NoError(t, err)
	assert.False(t, os.SameFile(st, byPath), "the presented file is not the one the path names now")
	_, err = f.Seek(0, io.SeekStart)
	require.NoError(t, err)
	got, err := io.ReadAll(f)
	require.NoError(t, err)
	assert.Equal(t, "the pinned kernel", string(got), "what is presented is what was hashed")
}

func TestPresent_AFileThatIsNotPinnedIsOpenedForItsPlaceOnly(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "disk")
	require.NoError(t, os.WriteFile(file, []byte("disk"), 0o600))

	for _, path := range []string{file, dir} {
		f, err := present(path, "")
		require.NoError(t, err, path)
		st, err := f.Stat()
		require.NoError(t, err)
		want, err := os.Stat(path)
		require.NoError(t, err)
		assert.True(t, os.SameFile(want, st), "%s is the file presented", path)
		f.Close()
	}
	_, err := present(filepath.Join(dir, "absent"), "")
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func TestPresent_APinnedPathMustBeARegularFile(t *testing.T) {
	_, err := present(t.TempDir(), sumOf(nil))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not a regular file")
}
