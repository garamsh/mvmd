//go:build e2e_real

package e2e_test

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/durationpb"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A Machine with a disk is hibernated and woken: it is paused, snapshotted, stopped and deleted,
// and the snapshot is restored as a new Machine, whose guest finds the disk with the marker the
// first guest wrote. The restore passes the node's check that the disk file shows the size and
// modification time the snapshot recorded, so ending a real VMM moved neither. The restored
// Machine detaches the disk, and a second restore, after the disk file was written from the host,
// is refused as precondition failed naming the disk as changed, with no Machine created.
func TestE2E_AMachineWithADiskIsHibernatedAndWokenWithTheDiskAndItsData(t *testing.T) {
	const (
		diskBytes = 64 << 20
		marker    = "written before the hibernation"
	)
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Contains(t, info.Drivers[0].Capabilities, v1alpha2.Capability_CAPABILITY_DISK_HOTPLUG, "the Driver reports the hot-plug capability a disk at Create needs")
	n.arm(t)
	pulled := n.pullBootable(t, "hibernate")

	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "extra", RequestId: "create-extra", SizeBytes: diskBytes})
	require.NoError(t, err)
	req := createRequest("origin", pulled.Digest)
	req.Disks = []*v1alpha2.DiskAttachment{{DiskId: "extra"}}
	n.runMachine(t, ctx, req)

	device := n.deviceOfSize(t, ctx, "origin", "origin", diskBytes)
	n.put(t, ctx, "origin", "write-marker", "/dev/"+device, marker)
	disk := filepath.Join(n.stateDir, "disks", "extra")
	written := statDisk(t, disk)
	require.Equal(t, int64(diskBytes), written.size, "the disk file is the size the disk was created with")
	head := make([]byte, len(marker))
	file, err := os.Open(disk)
	require.NoError(t, err)
	_, err = file.ReadAt(head, 0)
	require.NoError(t, err)
	require.NoError(t, file.Close())
	require.Equal(t, marker, string(head), "the guest's write reached the disk file, so the restored guest has something to read")

	_, err = n.client.PauseMachine(ctx, &v1alpha2.PauseMachineRequest{MachineId: "origin"})
	require.NoError(t, err)
	_, err = n.client.SnapshotMachine(ctx, &v1alpha2.SnapshotMachineRequest{SnapshotId: "snap", RequestId: "snapshot-snap", MachineId: "origin"})
	require.NoError(t, err)
	snapshotted := statDisk(t, disk)
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "origin", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	stopped := statDisk(t, disk)
	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "origin"})
	require.NoError(t, err)
	deleted := statDisk(t, disk)
	assert.Equal(t, map[string]string{"extra": ""}, n.holders(t, ctx), "the deleted Machine left the disk with no holder, and its root disk with it")

	restoreCtx, cancelRestore := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelRestore()
	restored, err := n.client.RestoreMachine(restoreCtx, &v1alpha2.RestoreMachineRequest{MachineId: "woken", RequestId: "restore-woken", SnapshotId: "snap-skipped"})
	// The stop rule of the check: a refusal as changed with nothing written names the step that
	// moved the size or the modification time, so every step's values are in the message.
	require.NoError(t, err, "the restore was refused; disk file after the write %+v, after the snapshot %+v, after the stop %+v, after the delete %+v", written, snapshotted, stopped, deleted)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, restored.Machine.State)
	require.Len(t, restored.Machine.Disks, 1, "the restored Machine holds the disk the snapshot recorded")
	assert.Equal(t, "extra", restored.Machine.Disks[0].DiskId)
	assert.Equal(t, map[string]string{"extra": "woken", "woken-root": "woken"}, n.holders(t, ctx), "the restored Machine holds the disk")

	assert.Equal(t, device, n.deviceOfSize(t, ctx, "woken", "woken", diskBytes), "the guest lists the disk as the origin's did")
	// The guest's cache of the disk is dropped, so the read below is of what the disk holds.
	n.put(t, ctx, "woken", "drop-caches", "/proc/sys/vm/drop_caches", "3")
	out, code := n.exec(t, ctx, "woken", "read-marker", "/bin/entry", "head", "/dev/"+device, strconv.Itoa(len(marker)))
	require.Equal(t, int32(0), code, "head of the disk: %s", out)
	assert.Equal(t, marker, out, "the restored guest reads the marker from the disk")

	before := n.blockDevices(t, ctx, "woken", "attached")
	detached, err := n.client.DetachDisk(ctx, &v1alpha2.DetachDiskRequest{MachineId: "woken", DiskId: "extra"})
	require.NoError(t, err)
	assert.Empty(t, detached.Machine.Disks)
	assert.Equal(t, []string{device}, difference(before, n.blockDevices(t, ctx, "woken", "detached")), "the device of the disk is the one gone")
	assert.Equal(t, map[string]string{"extra": "", "woken-root": "woken"}, n.holders(t, ctx), "the disk has no holder again, so a refusal below is not the holder's")

	// The control: the check is live. The disk is the node's, with no holder and as the snapshot
	// recorded it but for a write of the host's, and a restore of the snapshot is refused for that.
	host, err := os.OpenFile(disk, os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = host.WriteAt([]byte("written by the host"), 4096)
	require.NoError(t, err)
	require.NoError(t, host.Close())
	require.NotEqual(t, deleted, statDisk(t, disk), "the host's write moved the disk file's size or modification time")
	_, err = n.client.RestoreMachine(restoreCtx, &v1alpha2.RestoreMachineRequest{MachineId: "refused", RequestId: "restore-refused", SnapshotId: "snap"})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err))
	assert.Contains(t, err.Error(), `disk "extra" is changed`, "the refusal names the disk and that it changed")
	assert.NotContains(t, n.listed(t, ctx), "refused", "the refused restore created no Machine")
	assert.Equal(t, map[string]string{"extra": "", "woken-root": "woken"}, n.holders(t, ctx), "the refused restore took no holder slot")
}

// diskFile is what the node's check compares of a disk file: its size and modification time.
type diskFile struct {
	size  int64
	mtime time.Time
}

func statDisk(t *testing.T, path string) diskFile {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return diskFile{size: info.Size(), mtime: info.ModTime()}
}

// deviceOfSize is the one block device of the guest with the given size in bytes.
func (n *node) deviceOfSize(t *testing.T, ctx context.Context, machine, id string, bytes int) string {
	t.Helper()
	sectors := strconv.Itoa(bytes/512) + "\n"
	listed := n.blockDevices(t, ctx, machine, id)
	var found []string
	for _, dev := range listed {
		if n.guestFile(t, ctx, machine, id+"-size-of-"+dev, "/sys/class/block/"+dev+"/size") == sectors {
			found = append(found, dev)
		}
	}
	require.Len(t, found, 1, "the guest lists one device of %d bytes: %v", bytes, listed)
	return found[0]
}

// put writes the text to the start of a file of the guest, which is a device or a kernel setting
// that WriteFile, which replaces a file by renaming over it, cannot write.
func (n *node) put(t *testing.T, ctx context.Context, machine, id, path, text string) {
	t.Helper()
	out, code := n.exec(t, ctx, machine, id, "/bin/entry", "put", path, text)
	require.Equal(t, int32(0), code, "put %s: %s", path, out)
}
