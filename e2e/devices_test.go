//go:build e2e_real

package e2e_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A disk the node holds, attached to a running Machine, is a block device its guest lists with
// the disk's size, and after Detach the guest lists the devices it listed before.
func TestE2E_AnAttachedDiskIsVisibleToTheGuestUntilItIsDetached(t *testing.T) {
	const diskBytes = 64 << 20
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "hosting")
	n.makeDisk(t, "extra", diskBytes, "hosting")
	before := n.blockDevices(t, ctx, "hosting", "before")
	require.NotEmpty(t, before, "the guest lists the devices of the disks it booted with")

	attached, err := n.client.AttachDisk(ctx, &v1alpha2.AttachDiskRequest{MachineId: "hosting", Disk: &v1alpha2.DiskAttachment{DiskId: "extra"}})
	require.NoError(t, err)
	assert.Equal(t, "extra", attached.Machine.Disks[0].DiskId)

	after := n.blockDevices(t, ctx, "hosting", "attached")
	added := difference(after, before)
	require.Len(t, added, 1, "the guest lists one device more than before: before %v, after %v", before, after)
	assert.Equal(t, strconv.Itoa(diskBytes/512)+"\n", n.guestFile(t, ctx, "hosting", "size-of-"+added[0], "/sys/class/block/"+added[0]+"/size"), "the device is the disk's size in sectors")
	assert.Empty(t, difference(before, after), "no device the guest had is gone")

	detached, err := n.client.DetachDisk(ctx, &v1alpha2.DetachDiskRequest{MachineId: "hosting", DiskId: "extra"})
	require.NoError(t, err)
	assert.Empty(t, detached.Machine.Disks)
	assert.Equal(t, before, n.blockDevices(t, ctx, "hosting", "detached"), "the guest lists the devices it listed before the attach")
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

// makeDisk makes a disk the node holds, a sparse ext4 file under <state_dir>/disks that the
// Machine's VMM user owns as it owns the Machine's root disk. The runtime API creates no other
// disk than a root disk (runtime-api.md, Open questions).
func (n *node) makeDisk(t *testing.T, id string, size int64, machine string) {
	t.Helper()
	path := filepath.Join(n.stateDir, "disks", id)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	require.NoError(t, err)
	require.NoError(t, file.Truncate(size))
	require.NoError(t, file.Close())
	out, err := exec.Command(mkfsExt4(), "-q", "-F", path).CombinedOutput()
	require.NoError(t, err, "mkfs.ext4: %s", out)
	root, err := os.Stat(filepath.Join(n.stateDir, "disks", machine+"-root"))
	require.NoError(t, err)
	owner := root.Sys().(*syscall.Stat_t)
	require.NoError(t, os.Chown(path, int(owner.Uid), int(owner.Gid)))
	require.NoError(t, os.Chmod(path, root.Mode().Perm()))
}

// guestFile reads a file of the guest through an exec of its entrypoint program.
func (n *node) guestFile(t *testing.T, ctx context.Context, machine, id, path string) string {
	t.Helper()
	out, code := n.exec(t, ctx, machine, id, "/bin/entry", "cat", path)
	require.Equal(t, int32(0), code, "cat %s: %s", path, out)
	return out
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
