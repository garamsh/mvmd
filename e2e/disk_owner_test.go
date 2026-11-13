//go:build e2e_real

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A fresh ext4 disk mounted at a path is given to the user the entrypoint runs as, so that user
// writes in it: a disk named at Create Machine is owned before the entrypoint starts, and one
// attached to the running Machine is owned at its mount. A Machine that runs as root leaves its
// disk as mkfs made it, which is the control for both.
func TestE2E_AFreshDiskMountedAtAPathIsGivenToANonRootEntrypointsUser(t *testing.T) {
	const diskBytes = 64 << 20
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullEntry(t, "owned", []string{"mount-check", "/data"})
	for _, id := range []string{"data", "hot", "rootdisk"} {
		_, err := n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: id, RequestId: "create-" + id, SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
		require.NoError(t, err)
	}

	// A disk named at Create Machine, as 1000:2000 with no passwd entry for either.
	req := createRequest("owned", pulled.Digest)
	req.User = "1000:2000"
	req.Disks = []*v1alpha2.DiskAttachment{{DiskId: "data", MountPath: "/data"}}
	n.runMachine(t, ctx, req)

	assert.Equal(t, "path=/data mounted=true ext4=true writable=true\n", n.entrypointOutput(t, ctx, "owned"), "the non-root entrypoint wrote in the disk when it started")
	out, code := n.exec(t, ctx, "owned", "stat-data", "/bin/entry", "stat", "/data")
	require.Equal(t, int32(0), code, "stat /data: %s", out)
	assert.Contains(t, out, "mode=0755 uid=1000 gid=2000", "the root is the entrypoint's, its mode as mkfs left it")
	out, code = n.exec(t, ctx, "owned", "stat-lost", "/bin/entry", "stat", "/data/lost+found")
	require.Equal(t, int32(0), code, "stat /data/lost+found: %s", out)
	assert.Contains(t, out, "uid=0 gid=0", "nothing under the root is changed")

	// A disk attached to the running Machine is owned at its mount, with the identity the boot
	// resolved.
	_, err := n.client.AttachDisk(ctx, &v1alpha2.AttachDiskRequest{MachineId: "owned", Disk: &v1alpha2.DiskAttachment{DiskId: "hot", MountPath: "/hot"}})
	require.NoError(t, err)
	out, code = n.exec(t, ctx, "owned", "stat-hot", "/bin/entry", "stat", "/hot")
	require.Equal(t, int32(0), code, "stat /hot: %s", out)
	assert.Contains(t, out, "mode=0755 uid=1000 gid=2000", "a disk attached while running is the entrypoint's too")

	// The control: a Machine that runs as root leaves a fresh disk as mkfs made it, so the lines
	// above are the agent's doing and not the filesystem's default.
	rooted := createRequest("rooted", pulled.Digest)
	rooted.User = "0:0"
	rooted.Disks = []*v1alpha2.DiskAttachment{{DiskId: "rootdisk", MountPath: "/data"}}
	n.runMachine(t, ctx, rooted)
	out, code = n.exec(t, ctx, "rooted", "stat-data", "/bin/entry", "stat", "/data")
	require.Equal(t, int32(0), code, "stat /data: %s", out)
	assert.Contains(t, out, "mode=0755 uid=0 gid=0", "a root entrypoint changes nothing")
}
