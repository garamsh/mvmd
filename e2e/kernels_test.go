//go:build e2e_real

package e2e_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// A node configured with two kernel names lists both in its runtime info, the default marked
// (ADR 0045).
func TestE2E_RuntimeInfoListsTheKernelsTheNodeOffers(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	info, err := n.client.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)

	var got []string
	for _, k := range info.GetKernels() {
		assert.Equal(t, os.Getenv(envKernelSHA), k.GetDigest(), "the kernel %q is listed with its digest", k.GetName())
		assert.Equal(t, k.GetName() == "default", k.GetIsDefault(), "only the default is marked: %q", k.GetName())
		got = append(got, k.GetName())
	}
	assert.Equal(t, []string{"alternate", "default"}, got)
}

// A Machine created with the name that is not the default records that name, boots, and reports
// the name and the digest of the file it booted; one created with no name records the default.
func TestE2E_AMachineCreatedWithANamedKernelBootsAndReportsIt(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "bootable")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	req := createRequest("named", pulled.Digest)
	req.Kernel = "alternate"
	_, err := n.client.CreateMachine(ctx, req)
	require.NoError(t, err)
	started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "named"})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)
	assert.Equal(t, "alternate", started.Machine.Kernel)
	assert.Equal(t, os.Getenv(envKernelSHA), started.Machine.KernelDigest)

	created, err := n.client.CreateMachine(ctx, createRequest("plain", pulled.Digest))
	require.NoError(t, err)
	assert.Equal(t, "default", created.Machine.Kernel, "a Create that names no kernel records the default")
}
