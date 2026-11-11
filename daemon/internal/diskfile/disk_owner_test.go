package diskfile_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func TestGiveAndReclaim_AnAbsentDiskIsAbsent(t *testing.T) {
	d, _ := newDisk(t)
	ctx := context.Background()

	require.ErrorIs(t, d.Give(ctx, "missing", machine.DiskOwner{ID: "m", UID: 1, GID: 1}), machine.ErrDiskAbsent)
	require.ErrorIs(t, d.Reclaim(ctx, "missing"), machine.ErrDiskAbsent)

	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "present", Request: "r", SizeBytes: 4 << 20}))
	require.NoError(t, d.Reclaim(ctx, "present"), "control: a disk the node holds is accepted")
}

func TestGiveAndReclaim_ChangeTheOwnerOfTheFile(t *testing.T) {
	if os.Geteuid() != 0 {
		// Giving a file to another user needs root, as the CreateRootFrom
		// ownership test does: re-run under passwordless sudo, and fail where
		// the host gives neither.
		self, err := os.Executable()
		require.NoError(t, err)
		out, err := exec.Command("sudo", "-n", self, "-test.v", "-test.count=1",
			"-test.run", "^TestGiveAndReclaim_ChangeTheOwnerOfTheFile$").CombinedOutput()
		require.NoError(t, err, "this test needs root or passwordless sudo, and this host gives neither: %s", out)
		require.Contains(t, string(out), "--- PASS: TestGiveAndReclaim_ChangeTheOwnerOfTheFile", "the run under sudo did not run the test: %s", out)
		return
	}
	d, dir := newDisk(t)
	ctx := context.Background()
	require.NoError(t, d.Create(ctx, machine.DiskEntry{ID: "data", Request: "r", SizeBytes: 4 << 20}))
	ownerOf := func() (uint32, uint32) {
		info, err := os.Stat(filepath.Join(dir, "data"))
		require.NoError(t, err)
		st, ok := info.Sys().(*syscall.Stat_t)
		require.True(t, ok)
		return st.Uid, st.Gid
	}
	uid, gid := ownerOf()
	require.Equal(t, uint32(os.Getuid()), uid, "setup: the file starts as mvmd's")

	require.NoError(t, d.Give(ctx, "data", machine.DiskOwner{ID: "m", UID: 23456, GID: 34567}))
	uid, gid = ownerOf()
	assert.Equal(t, uint32(23456), uid)
	assert.Equal(t, uint32(34567), gid)

	require.NoError(t, d.Reclaim(ctx, "data"))
	uid, gid = ownerOf()
	assert.Equal(t, uint32(os.Getuid()), uid)
	assert.Equal(t, uint32(os.Getgid()), gid)
}
