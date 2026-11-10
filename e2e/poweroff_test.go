//go:build firecracker_real

package e2e_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// A Machine whose entrypoint ends by itself with a non-zero code is powered off by the Guest
// Agent: its VMM ends, and the daemon reports the entrypoint's code, read after the end with no
// stream open on the record, from the run directory's log.
func TestE2E_AnEntrypointThatEndsPowersTheMachineOff(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullEntry(t, "ends", []string{"exit", "7"})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, err := n.client.CreateMachine(ctx, createRequest("ends", pulled.Digest))
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "ends"})
	require.NoError(t, err)

	n.waitVMMEnded(t, "ends", 2*time.Minute)

	// The daemon learns nothing of the end until a call reaches the Machine, so the state it
	// reports is the one it recorded at Start.
	inspected, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "ends"})
	require.NoError(t, err)
	t.Logf("the state the runtime API reports after the VMM ended: %s", inspected.Machine.State)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, inspected.Machine.State)

	// This is the first read of the record: no stream was open on it while the Machine ran.
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

	// The log the code came from is whole: the agent's line is in it once the VMM has ended.
	log, err := os.ReadFile(filepath.Join(n.stateDir, "machines", "ends", "run", "vmm.log"))
	require.NoError(t, err)
	t.Logf("vmm.log of the ended Machine:\n%s", log)
	assert.Contains(t, string(log), "mvmd-guest-agent: entrypoint exited code=7")
}

// waitVMMEnded returns once the Machine's supervisor, which ends when its VMM has, no longer
// accepts a connection on its socket in the Machine's state directory. The socket file stays
// behind, so a refused connection is the end and the file's presence is not.
func (n *node) waitVMMEnded(t *testing.T, machine string, within time.Duration) {
	t.Helper()
	socket := filepath.Join(n.stateDir, "machines", machine, "supervisor.sock")
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("unix", socket, 5*time.Second)
		if err == nil {
			_ = conn.Close()
			time.Sleep(250 * time.Millisecond)
			continue
		}
		require.True(t, errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, os.ErrNotExist), "dial the supervisor of %s: %v", machine, err)
		return
	}
	t.Fatalf("the VMM of %s had not ended %s after Start", machine, within)
}
