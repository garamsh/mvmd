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
