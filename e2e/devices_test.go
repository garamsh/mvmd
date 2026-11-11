//go:build e2e_real

package e2e_test

import (
	"context"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A disk made by Create disk is listed with no holder, is a block device of the disk's size in
// the guest of the Machine it is attached to, which List disks then names as its holder, and is
// not deletable while attached. After Detach the guest lists the devices it listed before, and
// Delete disk removes it. Both Drivers hot-plug a disk, so neither is expected to refuse.
func TestE2E_ADiskMadeByCreateDiskIsAttachedToAGuestAndDetachedAgain(t *testing.T) {
	const diskBytes = 64 << 20
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Contains(t, info.Drivers[0].Capabilities, v1alpha2.Capability_CAPABILITY_DISK_HOTPLUG, "the Driver reports the hot-plug capability the attach below needs")
	n.startRunning(t, ctx, "hosting")

	create := &v1alpha2.CreateDiskRequest{DiskId: "extra", RequestId: "create-extra", SizeBytes: diskBytes}
	_, err = n.client.CreateDisk(ctx, create)
	require.NoError(t, err)
	_, err = n.client.CreateDisk(ctx, create)
	assert.NoError(t, err, "the same request again succeeds")
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "extra", RequestId: "create-extra-again", SizeBytes: diskBytes})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT, v1alpha2.KindOf(err), "the same identifier under another request ID")
	assert.Equal(t, map[string]string{"extra": "", "hosting-root": "hosting"}, n.holders(t, ctx), "the disk has no holder, and the Machine's root disk has the Machine")
	assert.Equal(t, uint64(diskBytes), n.diskSizes(t, ctx)["extra"])

	before := n.blockDevices(t, ctx, "hosting", "before")
	require.NotEmpty(t, before, "the guest lists the devices of the disks it booted with")
	attached, err := n.client.AttachDisk(ctx, &v1alpha2.AttachDiskRequest{MachineId: "hosting", Disk: &v1alpha2.DiskAttachment{DiskId: "extra"}})
	require.NoError(t, err)
	require.Len(t, attached.Machine.Disks, 1)
	assert.Equal(t, "extra", attached.Machine.Disks[0].DiskId)

	after := n.blockDevices(t, ctx, "hosting", "attached")
	added := difference(after, before)
	require.Len(t, added, 1, "the guest lists one device more than before: before %v, after %v", before, after)
	assert.Equal(t, strconv.Itoa(diskBytes/512)+"\n", n.guestFile(t, ctx, "hosting", "size-of-"+added[0], "/sys/class/block/"+added[0]+"/size"), "the device is the disk's size in sectors")
	assert.Empty(t, difference(before, after), "no device the guest had is gone")
	assert.Equal(t, map[string]string{"extra": "hosting", "hosting-root": "hosting"}, n.holders(t, ctx), "the Machine holds the disk")

	// The control: the refusal is the holder's, since the same call succeeds below once the disk
	// is detached.
	_, err = n.client.DeleteDisk(ctx, &v1alpha2.DeleteDiskRequest{DiskId: "extra"})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err), "a disk a Machine holds")
	assert.Contains(t, n.holders(t, ctx), "extra", "the refused call removed nothing")

	detached, err := n.client.DetachDisk(ctx, &v1alpha2.DetachDiskRequest{MachineId: "hosting", DiskId: "extra"})
	require.NoError(t, err)
	assert.Empty(t, detached.Machine.Disks)
	assert.Equal(t, before, n.blockDevices(t, ctx, "hosting", "detached"), "the guest lists the devices it listed before the attach")
	assert.Equal(t, "", n.holders(t, ctx)["extra"], "the disk has no holder again")

	_, err = n.client.DeleteDisk(ctx, &v1alpha2.DeleteDiskRequest{DiskId: "extra"})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"hosting-root": "hosting"}, n.holders(t, ctx), "the deleted disk is no longer listed")
}

// SetResources within the ceiling changes the vCPUs and memory the guest reports, and one above
// it is rejected as invalid and changes nothing. A Driver that cannot resize a running Machine
// refuses it as unsupported, with the guest as it was.
func TestE2E_SetResourcesChangesWhatTheGuestReportsWithinTheCeiling(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	// The Drivers that resize a running Machine are the Cloud Hypervisor one on x86_64: the
	// capabilities the node reports are checked against that, so a node that reported one it
	// did not have, or none it had, is not taken for the other case.
	resizes := driver.driver == "cloud-hypervisor" && runtime.GOARCH == "amd64"
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	reported := info.Drivers[0].Capabilities
	require.Equal(t, resizes, slices.Contains(reported, v1alpha2.Capability_CAPABILITY_CPU_RESIZE), "the CPU resize capability the Driver reports")
	require.Equal(t, resizes, slices.Contains(reported, v1alpha2.Capability_CAPABILITY_MEMORY_RESIZE), "the memory resize capability the Driver reports")

	n.arm(t)
	pulled := n.pullBootable(t, "resized")
	req := createRequest("resized", pulled.Digest)
	if resizes {
		req.ResourceCeiling = &v1alpha2.Resources{CpuCount: 2, MemoryBytes: 512 << 20}
	}
	n.runMachine(t, ctx, req)
	start := &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20}
	require.Equal(t, "0\n", n.guestFile(t, ctx, "resized", "cpus-before", "/sys/devices/system/cpu/online"), "the guest booted with one vCPU")
	memoryBefore := n.guestMemoryBytes(t, ctx, "resized", "memory-before")

	setCtx, cancelSet := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelSet()
	target := &v1alpha2.Resources{CpuCount: 2, MemoryBytes: 512 << 20}
	if !resizes {
		// A target of less memory than the one the Machine has, which no ceiling refuses.
		_, err = n.client.SetResources(setCtx, &v1alpha2.SetResourcesRequest{MachineId: "resized", Target: &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 128 << 20}})
		require.Error(t, err)
		assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_UNSUPPORTED, v1alpha2.KindOf(err), "the Driver cannot resize a running Machine")
	} else {
		set, err := n.client.SetResources(setCtx, &v1alpha2.SetResourcesRequest{MachineId: "resized", Target: target})
		require.NoError(t, err)
		assert.Equal(t, target.CpuCount, set.Resources.CpuCount)
		assert.Equal(t, target.MemoryBytes, set.Resources.MemoryBytes)
		start = target
		assert.Equal(t, "0-1\n", n.guestFile(t, ctx, "resized", "cpus-after", "/sys/devices/system/cpu/online"), "the guest has the vCPUs online")
		memoryAfter := n.guestMemoryBytes(t, ctx, "resized", "memory-after")
		assert.Greater(t, memoryAfter, memoryBefore+200<<20, "the guest reports the memory added")
		assert.Less(t, memoryAfter, memoryBefore+320<<20, "the guest reports no more than the memory added")
	}
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "resized"})
	require.NoError(t, err)
	assert.Equal(t, start.CpuCount, inspected.Machine.Resources.CpuCount)
	assert.Equal(t, start.MemoryBytes, inspected.Machine.Resources.MemoryBytes)

	// Above the ceiling, which is the resources' where none was given: invalid, and not the
	// unsupported the control above answers, so the ceiling is what refuses.
	_, err = n.client.SetResources(setCtx, &v1alpha2.SetResourcesRequest{MachineId: "resized", Target: &v1alpha2.Resources{CpuCount: 3, MemoryBytes: start.MemoryBytes}})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "a target above the ceiling")
	inspected, err = n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "resized"})
	require.NoError(t, err)
	assert.Equal(t, start.CpuCount, inspected.Machine.Resources.CpuCount, "the refused call changed nothing")
	assert.Equal(t, start.MemoryBytes, inspected.Machine.Resources.MemoryBytes, "the refused call changed nothing")
}

// holders is what List disks answers: each disk's holding Machine by the disk's identifier, empty
// for a disk no Machine holds.
func (n *node) holders(t *testing.T, ctx context.Context) map[string]string {
	t.Helper()
	held := map[string]string{}
	for _, d := range n.listedDisks(t, ctx) {
		held[d.Id] = d.HolderMachineId
	}
	return held
}

// diskSizes is what List disks answers: each disk's size in bytes by its identifier.
func (n *node) diskSizes(t *testing.T, ctx context.Context) map[string]uint64 {
	t.Helper()
	sizes := map[string]uint64{}
	for _, d := range n.listedDisks(t, ctx) {
		sizes[d.Id] = d.SizeBytes
	}
	return sizes
}

func (n *node) listedDisks(t *testing.T, ctx context.Context) []*v1alpha2.Disk {
	t.Helper()
	listed, err := n.client.ListDisks(ctx, &v1alpha2.ListDisksRequest{})
	require.NoError(t, err)
	return listed.Disks
}

// blockDevices lists the block devices the guest has, sorted.
func (n *node) blockDevices(t *testing.T, ctx context.Context, machine, id string) []string {
	t.Helper()
	out, code := n.exec(t, ctx, machine, "devices-"+id, "/bin/entry", "ls", "/sys/class/block")
	require.Equal(t, int32(0), code, "ls /sys/class/block: %s", out)
	names := strings.Fields(out)
	slices.Sort(names)
	return names
}

// difference lists what a holds and b does not.
func difference(a, b []string) []string {
	var only []string
	for _, name := range a {
		if !slices.Contains(b, name) {
			only = append(only, name)
		}
	}
	return only
}

// guestFile reads a file of the guest through an exec of its entrypoint program.
func (n *node) guestFile(t *testing.T, ctx context.Context, machine, id, path string) string {
	t.Helper()
	out, code := n.exec(t, ctx, machine, id, "/bin/entry", "cat", path)
	require.Equal(t, int32(0), code, "cat %s: %s", path, out)
	return out
}

// guestMemoryBytes is the memory the guest's kernel reports as its own.
func (n *node) guestMemoryBytes(t *testing.T, ctx context.Context, machine, id string) uint64 {
	t.Helper()
	for _, line := range strings.Split(n.guestFile(t, ctx, machine, id, "/proc/meminfo"), "\n") {
		if fields := strings.Fields(line); len(fields) == 3 && fields[0] == "MemTotal:" {
			kib, err := strconv.ParseUint(fields[1], 10, 64)
			require.NoError(t, err)
			return kib << 10
		}
	}
	t.Fatal("/proc/meminfo of the guest has no MemTotal")
	return 0
}
