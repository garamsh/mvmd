//go:build firecracker_real

package e2e_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// A Machine whose entrypoint ends by itself with a non-zero code is ended by the Guest Agent by
// the method its Driver names, and the daemon moves it to Stopped with the reason and reads the
// entrypoint's code from the run directory's log with no stream open on the record.
func TestE2E_AnEntrypointThatEndsStopsTheMachineWithItsExitCode(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntry(t, "ends", []string{"exit", "7"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, err := n.client.CreateMachine(ctx, createRequest("ends", pulled.Digest))
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "ends"})
	require.NoError(t, err)

	// The wait is on the state the test asserts, and no call but Inspect reaches the Machine
	// meanwhile, so the daemon's own look at the VMM is what moves it.
	var machine *v1alpha1.Machine
	deadline := time.Now().Add(2 * time.Minute)
	for {
		inspected, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "ends"})
		require.NoError(t, err)
		machine = inspected.Machine
		if machine.State == v1alpha1.MachineState_MACHINE_STATE_STOPPED || time.Now().After(deadline) {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	require.Equal(t, v1alpha1.MachineState_MACHINE_STATE_STOPPED, machine.State,
		"the Machine's state 2m0s after Start, last failure reason %q", machine.LastFailureReason)
	assert.Equal(t, "the VMM ended with no call asking it to: the entrypoint exited with code 7", machine.LastFailureReason)

	// This is the first read of the record: no stream was open on it while the Machine ran, so the
	// code comes from the agent's line in the log of the VMM that has ended.
	readCtx, cancelRead := context.WithTimeout(ctx, time.Minute)
	defer cancelRead()
	stream, err := n.client.ReadExecOutput(readCtx, &v1alpha1.ReadExecOutputRequest{MachineId: "ends", ExecId: "entrypoint"})
	require.NoError(t, err)
	var record *v1alpha1.ExecRecord
	for record == nil {
		part, err := stream.Recv()
		require.NoError(t, err, "the entrypoint's record of an ended Machine")
		record = part.GetRecord()
	}
	assert.Equal(t, v1alpha1.ExecStatus_EXEC_STATUS_EXITED, record.Status, record.Reason)
	assert.Equal(t, int32(7), record.ExitCode)
}
