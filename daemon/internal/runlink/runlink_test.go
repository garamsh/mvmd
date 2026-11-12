package runlink_test

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/runlink"
)

func inode(t *testing.T, path string) uint64 {
	t.Helper()
	var st syscall.Stat_t
	require.NoError(t, syscall.Stat(path, &st))
	return st.Ino
}

func TestCheck_RefusesANameThatIsNotOneFileNameAndAPathThatIsNotAbsolute(t *testing.T) {
	abs := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(abs, []byte("data"), 0o600))
	tests := []struct {
		label, name, path, want string
	}{
		{"empty name", "", abs, "not a file name"},
		{"dot", ".", abs, "not a file name"},
		{"dot dot", "..", abs, "not a file name"},
		{"slash", "a/b", abs, "not a file name"},
		{"leading dots", "../escape", abs, "not a file name"},
		{"relative path", "ok", "file", "not absolute"},
		{"empty path", "ok", "", "not absolute"},
	}
	for _, tt := range tests {
		err := runlink.Check(tt.name, tt.path)
		require.Error(t, err, tt.label)
		assert.Contains(t, err.Error(), tt.want, tt.label)
	}
	assert.NoError(t, runlink.Check("ok", abs), "control: one file name and an absolute path")
}

func TestCheck_RefusesAPathThatIsAbsentOrNotARegularFile(t *testing.T) {
	root := t.TempDir()
	regular := filepath.Join(root, "regular")
	require.NoError(t, os.WriteFile(regular, []byte("data"), 0o600))
	symlink := filepath.Join(root, "symlink")
	require.NoError(t, os.Symlink(regular, symlink))
	tests := []struct {
		label, path, want string
	}{
		{"absent", filepath.Join(root, "absent"), "no such file"},
		{"a symbolic link to a regular file", symlink, "not a regular file"},
		{"a directory", root, "not a regular file"},
	}
	for _, tt := range tests {
		err := runlink.Check("ok", tt.path)
		require.Error(t, err, tt.label)
		assert.Contains(t, err.Error(), tt.want, tt.label)
	}
	assert.NoError(t, runlink.Check("ok", regular), "control: a regular file")
}

func TestMake_HardLinksTheFileUnderTheName(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o600))
	run := t.TempDir()

	require.NoError(t, runlink.Make(run, "linked", src))
	assert.Equal(t, inode(t, src), inode(t, filepath.Join(run, "linked")), "a hard link, not a copy")
}

func TestMake_ReplacesWhatStandsAtTheNameWithoutFollowingIt(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o600))
	other := filepath.Join(t.TempDir(), "other")
	require.NoError(t, os.WriteFile(other, []byte("other"), 0o600))
	run := t.TempDir()
	// A symbolic link at the name: following it would write through to other.
	require.NoError(t, os.Symlink(other, filepath.Join(run, "linked")))
	before := inode(t, other)
	require.NotEqual(t, inode(t, src), before, "setup: the leftover points at another file")

	require.NoError(t, runlink.Make(run, "linked", src))

	info, err := os.Lstat(filepath.Join(run, "linked"))
	require.NoError(t, err)
	assert.True(t, info.Mode().IsRegular(), "the leftover link is gone")
	assert.Equal(t, inode(t, src), inode(t, filepath.Join(run, "linked")))
	got, err := os.ReadFile(other)
	require.NoError(t, err)
	assert.Equal(t, "other", string(got), "what the leftover pointed at is untouched")
}

func TestMake_ALinkThatCannotBeMadeNamesTheEntry(t *testing.T) {
	run := t.TempDir()
	err := runlink.Make(run, "second", filepath.Join(t.TempDir(), "absent"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "second")
	assert.NoFileExists(t, filepath.Join(run, "second"))

	// Control: the same name with a file that exists is linked.
	src := filepath.Join(t.TempDir(), "src")
	require.NoError(t, os.WriteFile(src, []byte("data"), 0o600))
	require.NoError(t, runlink.Make(run, "second", src))
	assert.FileExists(t, filepath.Join(run, "second"))
}
