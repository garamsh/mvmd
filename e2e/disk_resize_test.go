//go:build e2e_real

package e2e_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A disk grows and the guest sees the new size (ADR 0055). Grown while its Machine is Stopped, the
// disk is seen at the new size after the next Start by both Drivers, which grow the file and leave
// the filesystem to the guest. Grown while the Machine runs, it is seen at the new size at once on
// a Driver that reports the disk-resize capability, and the call is refused on one that does not.
// The sizes are the ones the guest reads from the device and the filesystem under the mount.
func TestE2E_AGrownDiskIsSeenAtItsNewSizeByTheGuest(t *testing.T) {
	const (
		first  = 64 << 20
		second = 128 << 20
		third  = 192 << 20
	)
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	caps := info.Drivers[0].Capabilities
	require.Contains(t, caps, v1alpha2.Capability_CAPABILITY_DISK_HOTPLUG, "the Driver reports the hot-plug capability a disk at Create needs")
	live := false
	for _, c := range caps {
		live = live || c == v1alpha2.Capability_CAPABILITY_DISK_RESIZE
	}
	n.arm(t)
	pulled := n.pullEntry(t, "grown", []string{"mount-check", "/data"})

	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "create-data", SizeBytes: first, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	req := createRequest("grown", pulled.Digest)
	req.Disks = []*v1alpha2.DiskAttachment{{DiskId: "data", MountPath: "/data"}}
	n.runMachine(t, ctx, req)

	// The sizes of the first start are the control: they are below what each growth is asserted to reach.
	fsBytes, deviceBytes := n.diskSizeInGuest(t, ctx, "grown", "start")
	assert.Equal(t, int64(first), deviceBytes, "the device is the disk's size")
	assert.Less(t, fsBytes, int64(first), "the filesystem fits in the device")

	if live {
		_, err = n.client.ResizeDisk(ctx, &v1alpha2.ResizeDiskRequest{DiskId: "data", SizeBytes: second})
		require.NoError(t, err)
		fsBytes, deviceBytes = n.diskSizeInGuest(t, ctx, "grown", "live")
		assert.Equal(t, int64(second), deviceBytes, "the guest sees the grown device without a restart")
		assert.Greater(t, fsBytes, int64(first), "the guest grew the filesystem on it")
		assert.Equal(t, uint64(second), n.diskSizes(t, ctx)["data"], "the node lists the new size")
	} else {
		_, err = n.client.ResizeDisk(ctx, &v1alpha2.ResizeDiskRequest{DiskId: "data", SizeBytes: second})
		require.Error(t, err)
		assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "a Driver without disk-resize refuses a live growth")
		assert.Equal(t, uint64(first), n.diskSizes(t, ctx)["data"], "a refused call leaves the disk")
	}
	// last is the size the disk has before the offline growth.
	last := int64(first)
	if live {
		last = second
	}

	// Offline, on every Driver: the Machine is Stopped, the file grows, and the next Start shows it.
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "grown", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	_, err = n.client.ResizeDisk(ctx, &v1alpha2.ResizeDiskRequest{DiskId: "data", SizeBytes: third})
	require.NoError(t, err)
	assert.Equal(t, uint64(third), n.diskSizes(t, ctx)["data"])
	_, err = n.client.ResizeDisk(ctx, &v1alpha2.ResizeDiskRequest{DiskId: "data", SizeBytes: third - 1})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "a disk does not shrink")
	started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "grown"})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)

	fsBytes, deviceBytes = n.diskSizeInGuest(t, ctx, "grown", "restart")
	assert.Equal(t, int64(third), deviceBytes, "the guest sees the disk grown while Stopped")
	assert.Greater(t, fsBytes, last, "and its filesystem grown to fill it")
}

// diskSizeInGuest runs the image's disk-size command on /data and returns the size of the
// filesystem and of the device the guest reports.
func (n *node) diskSizeInGuest(t *testing.T, ctx context.Context, machine, id string) (fsBytes, deviceBytes int64) {
	t.Helper()
	out, code := n.exec(t, ctx, machine, id, "/bin/entry", "disk-size", "/data")
	require.Equal(t, int32(0), code, "disk-size /data: %s", out)
	var path string
	_, err := fmt.Sscanf(out, "path=%s fs_bytes=%d device_bytes=%d", &path, &fsBytes, &deviceBytes)
	require.NoError(t, err, "the output of disk-size: %q", out)
	return fsBytes, deviceBytes
}
