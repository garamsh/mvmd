package diskfile_test

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/diskfile"
)

const mkfsPath = "/usr/sbin/mkfs.ext4"
const blkidPath = "/usr/sbin/blkid"

func owner(id string) machine.DiskOwner {
	return machine.DiskOwner{
		ID:  machine.MachineID(id),
		UID: uint32(os.Getuid()),
		GID: uint32(os.Getgid()),
	}
}

// TestCreateRoot_Sparse asserts the file is sparse: allocated blocks
// are far below the requested size.
func TestCreateRoot_Sparse(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	const size = 1 << 30 // 1 GiB
	owner := owner("m1")
	diskID, uuid, err := d.CreateRoot(context.Background(), owner, size)
	require.NoError(t, err)
	assert.NotEmpty(t, uuid)
	assert.Equal(t, machine.DiskID("m1-root"), diskID)

	path := filepath.Join(dir, string(diskID))
	info, err := os.Stat(path)
	require.NoError(t, err)

	var blocks uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		blocks = uint64(st.Blocks) * 512
	}
	assert.True(t, blocks < size/10,
		"sparse: allocated %d bytes far below size %d", blocks, size)
}

// TestCreateRoot_UUID asserts blkid prints the returned UUID.
func TestCreateRoot_UUID(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	owner := owner("m2")
	diskID, wantUUID, err := d.CreateRoot(context.Background(), owner, 1<<20)
	require.NoError(t, err)

	out, err := exec.Command(blkidPath, "-o", "value", "-s", "UUID",
		filepath.Join(dir, string(diskID))).CombinedOutput()
	require.NoError(t, err, "blkid output: %s", string(out))
	assert.Equal(t, wantUUID, string(bytes.TrimSpace(out)))
}

// TestCreateRoot_FailingMKFS asserts a bad mkfs path returns ErrBadMKFS.
func TestCreateRoot_FailingMKFS(t *testing.T) {
	dir := t.TempDir()
	_, err := diskfile.NewDisk(dir, "/usr/sbin/nonexistent-mkfs")
	require.Error(t, err)
	require.Contains(t, err.Error(), "mkfs.ext4 not available")
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Empty(t, entries, "no files after failed mkfs check")
}

// TestCreateRoot_InvalidDiskName rejects unsafe names.
func TestCreateRoot_InvalidDiskName(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	for _, id := range []string{"../evil", "./bad", "has/slash"} {
		_, _, err := d.CreateRoot(context.Background(),
			machine.DiskOwner{ID: machine.MachineID(id), UID: uint32(os.Getuid()), GID: uint32(os.Getgid())},
			1<<20)
		assert.Error(t, err, "id %q should be rejected", id)
	}
}

// TestCreateRoot_SingleFile asserts a successful CreateRoot leaves
// exactly one file in the directory.
func TestCreateRoot_SingleFile(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	owner := owner("m3")
	diskID, _, err := d.CreateRoot(context.Background(), owner, 1<<20)
	require.NoError(t, err)

	// The file should exist
	_, err = os.Stat(filepath.Join(dir, string(diskID)))
	require.NoError(t, err)

	// And no other files
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "only the root disk file")
}

// TestRemove_AbsentSucceeds asserts removing an absent disk is success.
func TestRemove_AbsentSucceeds(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	err = d.Remove(context.Background(), "nonexistent-root")
	assert.NoError(t, err)
}

// TestLocate_AbsentReturnsErrDiskAbsent asserts Locate returns ErrDiskAbsent
// for a disk the node does not hold.
func TestLocate_AbsentReturnsErrDiskAbsent(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	_, err = d.Locate(context.Background(), "missing-root")
	require.ErrorIs(t, err, machine.ErrDiskAbsent)
}

// TestOwner asserts chown was applied: the file is owned by the given UID/GID.
func TestOwner(t *testing.T) {
	dir := t.TempDir()
	d, err := diskfile.NewDisk(dir, mkfsPath)
	require.NoError(t, err)

	uid := uint32(os.Getuid())
	gid := uint32(os.Getgid())
	owner := machine.DiskOwner{ID: "m4", UID: uid, GID: gid}

	diskID, _, err := d.CreateRoot(context.Background(), owner, 1<<20)
	require.NoError(t, err)

	info, err := os.Stat(filepath.Join(dir, string(diskID)))
	require.NoError(t, err)

	st, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok, "stat returned a syscall.Stat_t")
	assert.Equal(t, uid, st.Uid)
	assert.Equal(t, gid, st.Gid)
}
