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

// stopGrace is the grace the stops below name, and stopForceBound is how long after the grace the
// forced stop may take to return: the VMM's end and the release of the Machine's network.
const (
	stopGrace      = 8 * time.Second
	stopForceBound = 15 * time.Second
)

// A Stop of a Machine whose entrypoint ignores SIGTERM returns Stopped at the end of the grace
// and not before it, with the VMM ended; the control, an entrypoint that ends on SIGTERM and is
// stopped with the same grace, returns Stopped before the grace elapses.
func TestE2E_AStopForcesAGuestThatIgnoresTheRequestDownAtTheEndOfTheGrace(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	stop := func(id string) time.Duration {
		before := time.Now()
		stopped, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: id, GracePeriod: durationpb.New(stopGrace)})
		elapsed := time.Since(before)
		require.NoError(t, err)
		require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, stopped.Machine.State)
		return elapsed
	}

	// The control comes first and is Running before it is stopped, as the other is.
	polite := n.pullEntry(t, "polite", nil)
	n.runMachine(t, ctx, createRequest("polite", polite.Digest))
	require.NotEmpty(t, n.vmmPIDs(t, "polite"), "the Machine's VMM runs before the stop")
	assert.Less(t, stop("polite"), stopGrace, "an entrypoint that ends on SIGTERM is stopped before the grace elapses")
	assert.Empty(t, n.vmmPIDs(t, "polite"), "the VMM has ended")

	stubborn := n.pullEntry(t, "stubborn", []string{"ignore-term"})
	n.runMachine(t, ctx, createRequest("stubborn", stubborn.Digest))
	// The entrypoint prints once SIGTERM is ignored, so the stop below meets a guest that does.
	require.Equal(t, "ignoring SIGTERM\n", n.entrypointOutput(t, ctx, "stubborn"))
	require.NotEmpty(t, n.vmmPIDs(t, "stubborn"), "the Machine's VMM runs before the stop")
	elapsed := stop("stubborn")
	assert.GreaterOrEqual(t, elapsed, stopGrace, "the stop of a guest that ignores the request waits out the grace")
	assert.Less(t, elapsed, stopGrace+stopForceBound, "and ends the VMM by force soon after it")
	assert.Empty(t, n.vmmPIDs(t, "stubborn"), "the VMM has ended")
}
