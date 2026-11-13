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

// An ext4 disk attached at a path of Create Machine is mounted there, and writable, when the
// entrypoint starts, and the node lists the disk's filesystem. A mount path on a disk with no
// filesystem is refused, and a path the guest does not mount is not a mount, which the same
// program reports of the root's /etc. Both Drivers hot-plug a disk at Start, so neither is
// expected to refuse.
func TestE2E_AnExt4DiskIsMountedAtItsPathWhenTheEntrypointStarts(t *testing.T) {
	const diskBytes = 64 << 20
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Contains(t, info.Drivers[0].Capabilities, v1alpha2.Capability_CAPABILITY_DISK_HOTPLUG, "the Driver reports the hot-plug capability a disk at Create needs")
	n.arm(t)
	pulled := n.pullEntry(t, "mounted", []string{"mount-check", "/data"})

	made, err := n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "create-data", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_EXT4, made.Disk.Filesystem)
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "raw", RequestId: "create-raw", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	assert.Equal(t, map[string]v1alpha2.Filesystem{"data": v1alpha2.Filesystem_FILESYSTEM_EXT4, "raw": v1alpha2.Filesystem_FILESYSTEM_NONE}, n.filesystems(t, ctx), "the listing reports each disk's filesystem")

	// A disk with no filesystem has nothing to mount: invalid, and the Machine is not made.
	refused := createRequest("refused", pulled.Digest)
	refused.Disks = []*v1alpha2.DiskAttachment{{DiskId: "raw", MountPath: "/data"}}
	_, err = n.client.CreateMachine(ctx, refused)
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "a mount path on a disk with no filesystem")

	req := createRequest("mounted", pulled.Digest)
	req.Disks = []*v1alpha2.DiskAttachment{{DiskId: "data", MountPath: "/data"}}
	n.runMachine(t, ctx, req)

	assert.Equal(t, "path=/data mounted=true ext4=true writable=true\n", n.entrypointOutput(t, ctx, "mounted"), "the entrypoint found the disk mounted and writable when it started")
	assert.Equal(t, "mounted", n.holders(t, ctx)["data"], "the Machine holds the disk")

	// The control: the program does not report every path as a mount, so the line above is the
	// disk's and not the program's default.
	out, code := n.exec(t, ctx, "mounted", "control", "/bin/entry", "mount-check-once", "/etc")
	require.Equal(t, int32(0), code, "mount-check-once /etc: %s", out)
	assert.Contains(t, out, "mounted=false", "a directory of the root is not a mount")
}

// filesystems is what List disks answers: each disk's filesystem by its identifier.
func (n *node) filesystems(t *testing.T, ctx context.Context) map[string]v1alpha2.Filesystem {
	t.Helper()
	got := map[string]v1alpha2.Filesystem{}
	for _, d := range n.listedDisks(t, ctx) {
		got[d.Id] = d.Filesystem
	}
	return got
}
