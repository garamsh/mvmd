//go:build e2e_real

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A disk made from a snapshot holds what the disk held when it was snapshotted (ADR 0056): a
// second Machine mounting the new disk reads the file the first wrote, after the snapshot is
// deleted.
func TestE2E_ADiskMadeFromASnapshotHoldsWhatTheSnapshottedDiskHeld(t *testing.T) {
	const diskBytes = 64 << 20
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Contains(t, info.Drivers[0].Capabilities, v1alpha2.Capability_CAPABILITY_DISK_HOTPLUG, "the Driver reports the hot-plug capability a disk at Create needs")
	n.arm(t)
	pulled := n.pullEntry(t, "snapshotted", []string{"mount-check", "/data"})

	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "create-data", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	writer := createRequest("writer", pulled.Digest)
	writer.Disks = []*v1alpha2.DiskAttachment{{DiskId: "data", MountPath: "/data"}}
	n.runMachine(t, ctx, writer)
	n.writeFile(t, ctx, "writer", "/data/marker", "written before the snapshot")
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "writer", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)

	snapshot := &v1alpha2.SnapshotDiskRequest{DiskId: "data", SnapshotId: "before", RequestId: "snap-before"}
	made, err := n.client.SnapshotDisk(ctx, snapshot)
	require.NoError(t, err)
	assert.Equal(t, "before", made.DiskSnapshot.Id)
	assert.Equal(t, "data", made.DiskSnapshot.DiskId)
	assert.Equal(t, uint64(diskBytes), made.DiskSnapshot.SizeBytes)
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_EXT4, made.DiskSnapshot.Filesystem)
	_, err = n.client.SnapshotDisk(ctx, snapshot)
	require.NoError(t, err, "the same call again succeeds")
	_, err = n.client.SnapshotDisk(ctx, &v1alpha2.SnapshotDiskRequest{DiskId: "data", SnapshotId: "before", RequestId: "another"})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, v1alpha2.KindOf(err), "the snapshot identifier is taken by another request")
	listed, err := n.client.ListDiskSnapshots(ctx, &v1alpha2.ListDiskSnapshotsRequest{})
	require.NoError(t, err)
	require.Len(t, listed.DiskSnapshots, 1, "the repeat made nothing again")

	copied, err := n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "copy", RequestId: "create-copy", SizeBytes: diskBytes, FromSnapshotId: "before"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_EXT4, copied.Disk.Filesystem, "the new disk takes the snapshot's filesystem")
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "small", RequestId: "create-small", SizeBytes: diskBytes - 1, FromSnapshotId: "before"})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "a disk is not smaller than its snapshot")

	// The snapshot is deleted: the disk made from it is its own.
	_, err = n.client.DeleteDiskSnapshot(ctx, &v1alpha2.DeleteDiskSnapshotRequest{SnapshotId: "before"})
	require.NoError(t, err)
	_, err = n.client.DeleteDiskSnapshot(ctx, &v1alpha2.DeleteDiskSnapshotRequest{SnapshotId: "before"})
	require.NoError(t, err, "an absent snapshot is deleted")
	listed, err = n.client.ListDiskSnapshots(ctx, &v1alpha2.ListDiskSnapshotsRequest{})
	require.NoError(t, err)
	assert.Empty(t, listed.DiskSnapshots)

	reader := createRequest("reader", pulled.Digest)
	reader.Disks = []*v1alpha2.DiskAttachment{{DiskId: "copy", MountPath: "/data"}}
	n.runMachine(t, ctx, reader)
	out, code := n.exec(t, ctx, "reader", "read", "/bin/entry", "cat", "/data/marker")
	require.Equal(t, int32(0), code, "cat /data/marker: %s", out)
	assert.Equal(t, "written before the snapshot", out, "the new disk holds what the disk held when it was snapshotted")

	// The control: the program does not print a marker for any path, so the output above is the
	// disk's and not the program's default.
	out, code = n.exec(t, ctx, "reader", "absent", "/bin/entry", "cat", "/data/absent")
	assert.NotEqual(t, int32(0), code, "a file the disk does not hold is not read: %s", out)
}
