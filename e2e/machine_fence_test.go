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

// renewFence renews the fence of one Machine and returns the status the call answers.
func (n *node) renewFence(t *testing.T, machine string, length time.Duration, number uint64) *v1alpha2.DeadlineStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	renewed, err := n.client.RenewDeadline(ctx, &v1alpha2.RenewDeadlineRequest{MachineId: machine, Length: durationpb.New(length), RenewalNumber: number})
	require.NoError(t, err)
	return renewed.Status
}

// A Machine created fenced is refused Start until its fence is renewed. When its fence lapses its
// supervisor ends its VMM, and no other Machine's VMM ends, whether mvmd runs or not. Inspect
// then shows the Machine Stopped, the reason naming its fence, and when it lapsed, also after
// mvmd was stopped and started again. The node's deadline is armed for an hour throughout.
func TestE2E_AMachinesFenceEndsOnlyItsOwnVMMWhenItLapses(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	for _, id := range []string{"long", "short", "down"} {
		req := createRequest(id, pulled.Digest)
		req.Fenced = true
		created, err := n.client.CreateMachine(ctx, req)
		require.NoError(t, err)
		require.True(t, created.Machine.Fenced, id)
	}
	_, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "long"})
	require.Error(t, err, "a Machine created fenced is not started while its fence is not armed")
	assert.Empty(t, n.vmmPIDs(t, "long"))
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED, n.fenceOf(t, "long").Phase)

	// Each fence is renewed just before its Machine's start; the lengths leave the starts time.
	for _, f := range []struct {
		id     string
		length time.Duration
	}{{"long", time.Hour}, {"short", 30 * time.Second}, {"down", 150 * time.Second}} {
		st := n.renewFence(t, f.id, f.length, 1)
		require.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, st.Phase, f.id)
		started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: f.id})
		require.NoError(t, err, f.id)
		require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State, f.id)
	}
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, n.deadline(t).Phase, "the node's deadline is not what the fences moved")

	// The first fence lapses with mvmd running.
	n.waitVMMGone(t, "short")
	n.waitState(t, ctx, "short", v1alpha2.MachineState_MACHINE_STATE_STOPPED)
	short := n.inspected(t, ctx, "short")
	assert.Contains(t, short.LastFailureReason, "own fence lapsed", "the reason names the Machine's fence")
	require.NotNil(t, short.FenceLapsedAt, "the time the fence lapsed is shown")
	assert.NotEmpty(t, n.vmmPIDs(t, "long"), "no other VMM ends")
	assert.NotEmpty(t, n.vmmPIDs(t, "down"), "no other VMM ends")
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, n.inspected(t, ctx, "long").State)

	// The second lapses while mvmd is not running: the supervisor's own count ends the VMM.
	n.stopDaemon()
	n.waitVMMGone(t, "down")
	assert.NotEmpty(t, n.vmmPIDs(t, "long"), "the other Machine's VMM runs on with mvmd stopped")
	n.startDaemon()
	down := n.inspected(t, ctx, "down")
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, down.State)
	assert.Contains(t, down.LastFailureReason, "own fence lapsed")
	require.NotNil(t, down.FenceLapsedAt)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, n.inspected(t, ctx, "long").State)
	assert.NotEmpty(t, n.vmmPIDs(t, "long"))
	assert.NotNil(t, n.inspected(t, ctx, "short").FenceLapsedAt, "the first lapse is still shown after the restart")

	// A renewal after the lapse arms the fence again and starts nothing.
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, n.renewFence(t, "short", time.Hour, 2).Phase)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, n.inspected(t, ctx, "short").State)
	assert.Empty(t, n.vmmPIDs(t, "short"))
}

// fenceOf reads one Machine's fence through Inspect deadline.
func (n *node) fenceOf(t *testing.T, machine string) *v1alpha2.DeadlineStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	inspected, err := n.client.InspectDeadline(ctx, &v1alpha2.InspectDeadlineRequest{MachineId: machine})
	require.NoError(t, err)
	return inspected.Status
}

// inspected reads the Machine through Inspect Machine.
func (n *node) inspected(t *testing.T, ctx context.Context, machine string) *v1alpha2.Machine {
	t.Helper()
	got, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: machine})
	require.NoError(t, err)
	return got.Machine
}
