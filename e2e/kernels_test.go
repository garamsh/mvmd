//go:build e2e_real

package e2e_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A node configured with two kernel names lists both in its runtime info, the default marked,
// each with the digest of its own file (ADR 0045).
func TestE2E_RuntimeInfoListsTheKernelsTheNodeOffers(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)

	digests := map[string]string{}
	for _, k := range info.GetKernels() {
		digests[k.GetName()] = k.GetDigest()
		assert.Equal(t, k.GetName() == "default", k.GetIsDefault(), "only the default is marked: %q", k.GetName())
	}
	assert.Equal(t, map[string]string{"default": os.Getenv(envKernelSHA), "alternate": n.alternateKernelSHA}, digests)
}

// A Machine created with the name that is not the default records that name and boots the file it
// maps to, and one created with no name records and boots the default. The record is the daemon's
// and the guest's own report of the kernel it runs is not: the alternate file differs from the
// default in the version its kernel reports, so each guest's answer says which file booted.
func TestE2E_AMachineCreatedWithANamedKernelBootsAndReportsIt(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "bootable")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	named := createRequest("named", pulled.Digest)
	named.Kernel = "alternate"
	_, err := n.client.CreateMachine(ctx, named)
	require.NoError(t, err)
	created, err := n.client.CreateMachine(ctx, createRequest("plain", pulled.Digest))
	require.NoError(t, err)
	assert.Equal(t, "default", created.Machine.Kernel, "a Create that names no kernel records the default")
	started := map[string]*v1alpha2.Machine{}
	for _, id := range []string{"named", "plain"} {
		resp, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: id})
		require.NoError(t, err)
		require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, resp.Machine.State)
		started[id] = resp.Machine
	}
	assert.Equal(t, "alternate", started["named"].Kernel)
	assert.Equal(t, n.alternateKernelSHA, started["named"].KernelDigest)
	assert.Equal(t, "default", started["plain"].Kernel)
	assert.Equal(t, os.Getenv(envKernelSHA), started["plain"].KernelDigest)

	version := func(id string) string {
		out, code := n.exec(t, ctx, id, "uname-"+id, "/bin/entry", "uname")
		require.Equal(t, int32(0), code)
		lines := strings.Split(strings.TrimSpace(out), "\n")
		require.Len(t, lines, 2, "the guest's release and version: %q", out)
		return lines[1]
	}
	plain := version("plain")
	require.True(t, strings.HasPrefix(plain, "#1 SMP "), "the default kernel reports its own version: %q", plain)
	assert.Equal(t, "#2"+strings.TrimPrefix(plain, "#1"), version("named"), "the guest of the Machine that names alternate runs the alternate file")
}
