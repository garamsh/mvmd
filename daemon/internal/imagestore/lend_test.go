package imagestore_test

import (
	"os"
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	converterUID = 23456
	converterGID = 34567
)

// ownerOf returns the owner and mode of the file.
func ownerOf(t *testing.T, path string) (uid, gid uint32, mode os.FileMode) {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	return st.Uid, st.Gid, info.Mode().Perm()
}

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
	args := []string{"-n"}
	if verityRoot != "" {
		// sudo drops the environment, so the file system found here is named
		// again for the run as root.
		args = append(args, "env", verityEnv+"="+verityRoot)
	}
	args = append(args, self, "-test.v", "-test.count=1", "-test.run", "^"+name+"$")
	out, err := exec.Command("sudo", args...).CombinedOutput()
	require.NoError(t, err, "this test needs root or passwordless sudo, and this host gives neither: %s", out)
	require.Contains(t, string(out), "--- PASS: "+name, "the run under sudo did not run the test: %s", out)
	return false
}

// A lent output is the user's, who can read and write it through the owner's
// bits, and no one else can through any bit.
func TestStore_LendGivesTheOutputToTheUser(t *testing.T) {
	if !runsAsRoot(t, "TestStore_LendGivesTheOutputToTheUser") {
		return
	}
	s, _ := newStore(t)
	staging, err := s.Stage(ctx, imageA, 8192)
	require.NoError(t, err)
	uid, gid, mode := ownerOf(t, staging.Output)
	assert.Equal(t, uint32(os.Geteuid()), uid, "control: the daemon owns the output as it is staged")
	assert.Equal(t, os.FileMode(0o600), mode)

	require.NoError(t, s.Lend(ctx, staging, machine.User{UID: converterUID, GID: converterGID}))

	uid, gid, mode = ownerOf(t, staging.Output)
	assert.Equal(t, uint32(converterUID), uid)
	assert.Equal(t, uint32(converterGID), gid)
	assert.Equal(t, os.FileMode(0o600), mode, "the mode is not loosened for the user")
}

// Reclaim takes the output back, and sets the mode the user may have changed.
// A Seal in between refuses a lent output, and succeeds on the reclaimed one.
func TestStore_ReclaimTakesTheOutputBackAndSealRefusesALentOne(t *testing.T) {
	if !runsAsRoot(t, "TestStore_ReclaimTakesTheOutputBackAndSealRefusesALentOne") {
		return
	}
	s, _ := newStore(t)
	staging, err := s.Stage(ctx, imageA, 8192)
	require.NoError(t, err)
	require.NoError(t, s.Lend(ctx, staging, machine.User{UID: converterUID, GID: converterGID}))
	require.NoError(t, os.Chmod(staging.Output, 0o666), "the user widens the mode")

	_, err = s.Seal(ctx, staging, 4096)
	require.Error(t, err, "a Seal of an output the converter's user still holds")
	assert.Contains(t, err.Error(), "still held")
	uid, _, _ := ownerOf(t, staging.Output)
	assert.Equal(t, uint32(converterUID), uid, "the refused Seal changed nothing")

	require.NoError(t, s.Reclaim(ctx, staging))

	uid, gid, mode := ownerOf(t, staging.Output)
	assert.Equal(t, uint32(os.Geteuid()), uid)
	assert.Equal(t, uint32(os.Getegid()), gid)
	assert.Equal(t, os.FileMode(0o600), mode)
	_, err = s.Seal(ctx, staging, 4096)
	require.NoError(t, err, "control: the reclaimed output is sealed")
}

// Reclaim sets the mode whoever the owner is, so it is checked without root.
func TestStore_ReclaimRestoresTheMode(t *testing.T) {
	s, _ := newStore(t)
	staging, err := s.Stage(ctx, imageA, 8192)
	require.NoError(t, err)
	require.NoError(t, os.Chmod(staging.Output, 0o666))

	require.NoError(t, s.Reclaim(ctx, staging))

	_, _, mode := ownerOf(t, staging.Output)
	assert.Equal(t, os.FileMode(0o600), mode)
}

// Only root may give a file to another user, so the refusal is only reachable
// without root; the test above and TestStore_LendGivesTheOutputToTheUser read
// the ownership instead.
func TestStore_LendFailsWhenTheOutputCannotBeGiven(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can chown to any user, so the Lend cannot fail here")
	}
	s, _ := newStore(t)
	staging, err := s.Stage(ctx, imageA, 8192)
	require.NoError(t, err)

	err = s.Lend(ctx, staging, machine.User{UID: uint32(os.Geteuid()) + 1, GID: uint32(os.Getegid())})

	require.Error(t, err)
	uid, _, _ := ownerOf(t, staging.Output)
	assert.Equal(t, uint32(os.Geteuid()), uid, "a Lend that failed gave nothing away")
	require.NoError(t, s.Lend(ctx, staging, machine.User{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}),
		"control: the caller's own user is accepted")
}

// A Lend and a Reclaim name a staging area this Store opened.
func TestStore_LendAndReclaimRefuseAForeignStagingArea(t *testing.T) {
	s, _ := newStore(t)
	staging, err := s.Stage(ctx, imageA, 8192)
	require.NoError(t, err)
	foreign := staging
	foreign.ID = "../../etc"

	user := machine.User{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	assert.Error(t, s.Lend(ctx, foreign, user))
	assert.Error(t, s.Reclaim(ctx, foreign))
	require.NoError(t, s.Lend(ctx, staging, user), "control: the staging area it opened")
	require.NoError(t, s.Reclaim(ctx, staging))
}
