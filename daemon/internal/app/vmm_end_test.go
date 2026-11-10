package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// A VMM that ends while the daemon serves is found by the wired daemon with no
// call: the Machine becomes Stopped with the reason, and the stream says so.
func TestVMMEnd_AMachineWhoseVMMEndsWhileServingBecomesStopped(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	populate(t, n)
	last := recvLast(t, n)

	n.app.Substitutes.Driver.WriteConsoleLog("a", "mvmd-guest-agent: entrypoint exited code=3\n")
	require.NoError(t, n.app.Substitutes.Supervisors.Terminate(ctx, "a"))

	require.Eventually(t, func() bool {
		a, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
		return err == nil && a.GetMachine().GetState() == v1alpha1.MachineState_MACHINE_STATE_STOPPED
	}, 10*time.Second, 50*time.Millisecond)
	a, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
	require.NoError(t, err)
	assert.Equal(t, "the VMM ended with no call asking it to: the entrypoint exited with code 3", a.GetMachine().GetLastFailureReason())

	e := recv(t, watch(t, n, last))
	assert.Equal(t, "a", e.GetMachineId())
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_STOPPED, e.GetStateChanged().GetState())

	b, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "b"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_CREATED, b.GetMachine().GetState())
}

// recvLast returns the sequence of the last event the node holds.
func recvLast(t *testing.T, n *node) uint64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	s, err := n.client.WatchEvents(ctx, &v1alpha1.WatchEventsRequest{})
	require.NoError(t, err)
	var last uint64
	for {
		e, err := s.Recv()
		if err != nil {
			return last
		}
		last = e.GetSequence()
	}
}
