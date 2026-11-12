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

// A Machine that was started, stopped and started again boots the second time, on either VMM: the
// run directory outlives the first VMM and holds its API socket, which the second must not find.
// The entrypoint prints its greeting at each boot, and the second is read as the second boot's
// output.
func TestE2E_AStoppedMachineStartsAgain(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "restart")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	n.runMachine(t, ctx, createRequest("again", pulled.Digest))
	stopped, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "again", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, stopped.Machine.State)

	started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "again"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)
	assert.Equal(t, "hello from the entrypoint\n", n.entrypointOutput(t, ctx, "again"), "the second boot ran the entrypoint")
}
