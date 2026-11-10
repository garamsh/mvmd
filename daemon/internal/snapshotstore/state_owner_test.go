package snapshotstore_test

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// originUID and originGID are an origin Machine's user, who is not the user
// running the tests.
const (
	originUID = 12345
	originGID = 12345
)

// runsAsRoot reports whether the caller should go on: it is root. A binary
// that is not root runs the named test alone again under passwordless sudo,
// which the GitHub-hosted runner provides, and the caller returns; where it
// can get neither, the test fails and is not skipped.
func runsAsRoot(t *testing.T, name string) bool {
	t.Helper()
	if os.Geteuid() == 0 {
		return true
	}
	self, err := os.Executable()
	require.NoError(t, err)
	out, err := exec.Command("sudo", "-n", self, "-test.v", "-test.count=1", "-test.run", "^"+name+"$").CombinedOutput()
	require.NoError(t, err, "this test needs root or passwordless sudo, and this host gives neither: %s", out)
	require.Contains(t, string(out), "--- PASS: "+name, "the run under sudo did not run the test: %s", out)
	return false
}

func ownerAndMode(t *testing.T, path string) (uid, gid uint32, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return st.Uid, st.Gid, info.Mode().Perm()
}

// A VMM writes its state files for its own user, mode 0600 or wider; a Put
// makes them the store's, 0444, so that the user of any restoring Machine can
// read them. Giving a file to another user needs root, so the owner is only
// read where the test runs as root.
func TestPut_PublishesStateFilesOwnedByTheStoreAndReadOnly(t *testing.T) {
	if !runsAsRoot(t, "TestPut_PublishesStateFilesOwnedByTheStoreAndReadOnly") {
		return
	}
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	for _, p := range files.State {
		require.NoError(t, os.Chown(p, originUID, originGID))
		require.NoError(t, os.Chmod(p, 0o600))
		uid, gid, mode := ownerAndMode(t, p)
		require.Equal(t, uint32(originUID), uid, "setup: the origin Machine's user owns %s", p)
		require.Equal(t, uint32(originGID), gid)
		require.Equal(t, os.FileMode(0o600), mode)
	}

	require.NoError(t, s.Put(ctx, snapshot("a"), files))

	_, got, err := s.Get(ctx, "a")
	require.NoError(t, err)
	require.Len(t, got.State, 2)
	for _, p := range got.State {
		uid, gid, mode := ownerAndMode(t, p)
		assert.Equal(t, uint32(os.Geteuid()), uid, "owner of %s", p)
		assert.Equal(t, uint32(os.Getegid()), gid, "group of %s", p)
		assert.Equal(t, os.FileMode(0o444), mode, "mode of %s", p)
	}
	// The root disk copy is the restored Machine's own copy and keeps the mode
	// the store made it with.
	_, _, mode := ownerAndMode(t, got.RootDisk)
	assert.Equal(t, os.FileMode(0o600), mode)
}

// The mode is set whoever owns the file, so it is read without root too.
func TestPut_PublishesStateFilesReadOnlyWhateverModeTheyHad(t *testing.T) {
	for _, mode := range []os.FileMode{0o600, 0o666, 0o400} {
		t.Run(mode.String(), func(t *testing.T) {
			f := newFixture(t)
			s := f.open(t)
			files := f.files(t, "a")
			for _, p := range files.State {
				require.NoError(t, os.Chmod(p, mode))
				_, _, got := ownerAndMode(t, p)
				require.Equal(t, mode, got, "setup: the mode the VMM left")
			}

			require.NoError(t, s.Put(ctx, snapshot("a"), files))

			_, got, err := s.Get(ctx, "a")
			require.NoError(t, err)
			for _, p := range got.State {
				_, _, m := ownerAndMode(t, p)
				assert.Equal(t, os.FileMode(0o444), m, "mode of %s", p)
			}
		})
	}
}

// A Driver writes its state files again over the ones a failed attempt left,
// as the user that wrote them, so a failed Put returns them with the owner
// and mode they had: a file left 0444 and the store's could not be opened for
// writing by that user. The failure is injected after the files were made the
// store's.
func requireAFailedPutReturnsTheOwnerAndMode(t *testing.T, owner bool) {
	t.Helper()
	f := newFixture(t)
	s := f.open(t)
	files := f.files(t, "a")
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if owner {
		uid, gid = originUID, originGID
	}
	for _, p := range files.State {
		require.NoError(t, os.Chown(p, int(uid), int(gid)))
		require.NoError(t, os.Chmod(p, 0o640))
	}
	s.SetFault(func(step string) error {
		if step == "sync-state" {
			return errInjected
		}
		return nil
	})

	require.ErrorIs(t, s.Put(ctx, snapshot("a"), files), errInjected)

	for _, p := range files.State {
		gotUID, gotGID, mode := ownerAndMode(t, p)
		assert.Equal(t, uid, gotUID, "owner of %s", p)
		assert.Equal(t, gid, gotGID, "group of %s", p)
		assert.Equal(t, os.FileMode(0o640), mode, "mode of %s", p)
	}
	requireNothingKept(t, f)
}

func TestPut_AFailedPutReturnsTheStateFilesWithTheirOwnerAndMode(t *testing.T) {
	if !runsAsRoot(t, "TestPut_AFailedPutReturnsTheStateFilesWithTheirOwnerAndMode") {
		return
	}
	requireAFailedPutReturnsTheOwnerAndMode(t, true)
}

// The mode is returned whoever owns the file, so it is read without root too.
func TestPut_AFailedPutReturnsTheStateFilesWithTheirMode(t *testing.T) {
	requireAFailedPutReturnsTheOwnerAndMode(t, false)
}
