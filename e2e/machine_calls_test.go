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

// A paused Machine resumes: the call answers Running, Inspect agrees, and Exec into the guest
// succeeds, where it was refused while the Machine was Paused.
func TestE2E_APausedMachineResumesAndExecSucceeds(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "napper")

	paused, err := n.client.PauseMachine(ctx, &v1alpha2.PauseMachineRequest{MachineId: "napper"})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_PAUSED, paused.Machine.State)
	// The control: the guest of a paused Machine takes no command, so the Exec below succeeds
	// because of the resume.
	_, err = n.client.Exec(ctx, &v1alpha2.ExecRequest{ExecId: "while-paused", RequestId: "exec-while-paused", MachineId: "napper", Command: []string{"/bin/entry", "echo", "paused"}})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err))

	resumed, err := n.client.ResumeMachine(ctx, &v1alpha2.ResumeMachineRequest{MachineId: "napper"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, resumed.Machine.State)
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "napper"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, inspected.Machine.State)

	// A guest that was not resumed never answers, so the exec is bounded well short of the
	// test's own time.
	execCtx, cancelExec := context.WithTimeout(ctx, time.Minute)
	defer cancelExec()
	out, code := n.exec(t, execCtx, "napper", "after-resume", "/bin/entry", "echo", "awake")
	assert.Equal(t, "awake\n", out)
	assert.Equal(t, int32(0), code)

	_, err = n.client.ResumeMachine(ctx, &v1alpha2.ResumeMachineRequest{MachineId: "napper"})
	assert.NoError(t, err, "a Machine that is already Running resumes with no change")
}

// Delete removes a Stopped Machine and its root disk's file from the state directory, leaves
// another Machine and its disk, and a repeat that finds the Machine absent succeeds.
func TestE2E_DeleteRemovesTheMachineAndItsRootDisk(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "deleted")
	n.runMachine(t, ctx, createRequest("doomed", pulled.Digest))
	_, err := n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "doomed", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)
	_, err = n.client.CreateMachine(ctx, createRequest("kept", pulled.Digest))
	require.NoError(t, err)
	require.ElementsMatch(t, []string{"doomed-root", "kept-root"}, n.entries(t, "disks"), "each Machine's root disk is a file under <state_dir>/disks")

	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "doomed"})
	require.NoError(t, err)

	assert.Equal(t, []string{"kept-root"}, n.entries(t, "disks"), "the deleted Machine's root disk is gone and the other's is not")
	_, err = n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "doomed"})
	require.Error(t, err)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, v1alpha2.KindOf(err))
	kept, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "kept"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_CREATED, kept.Machine.State)

	_, err = n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "doomed"})
	assert.NoError(t, err, "a Machine that is absent is deleted with no change")
}

// List holds every Machine the test created with the state each is in, nothing before the first
// and nothing of one that was deleted.
func TestE2E_ListMachinesListsTheMachinesCreated(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "listed")
	// The control: the node held no Machine before the test made these.
	require.Empty(t, n.listed(t, ctx))
	n.runMachine(t, ctx, createRequest("running", pulled.Digest))
	for _, id := range []string{"created-1", "created-2"} {
		_, err := n.client.CreateMachine(ctx, createRequest(id, pulled.Digest))
		require.NoError(t, err)
	}

	assert.Equal(t, map[string]v1alpha2.MachineState{
		"running":   v1alpha2.MachineState_MACHINE_STATE_RUNNING,
		"created-1": v1alpha2.MachineState_MACHINE_STATE_CREATED,
		"created-2": v1alpha2.MachineState_MACHINE_STATE_CREATED,
	}, n.listed(t, ctx))

	_, err := n.client.DeleteMachine(ctx, &v1alpha2.DeleteMachineRequest{MachineId: "created-1"})
	require.NoError(t, err)
	assert.Equal(t, map[string]v1alpha2.MachineState{
		"running":   v1alpha2.MachineState_MACHINE_STATE_RUNNING,
		"created-2": v1alpha2.MachineState_MACHINE_STATE_CREATED,
	}, n.listed(t, ctx), "a deleted Machine is not listed")
}

// Watch delivers a Machine's state changes in the order they were made, across its creation, a
// start and a stop, each with a sequence number above the one before.
func TestE2E_WatchEventsDeliversAMachinesStateChangesInOrder(t *testing.T) {
	n := startNode(t)
	n.arm(t)
	pulled := n.pullBootable(t, "watched")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	watchCtx, stopWatching := context.WithCancel(ctx)
	defer stopWatching()
	stream, err := n.client.WatchEvents(watchCtx, &v1alpha2.WatchEventsRequest{})
	require.NoError(t, err)
	type received struct {
		event *v1alpha2.WatchEventsResponse
		err   error
	}
	events := make(chan received)
	go func() {
		for {
			event, err := stream.Recv()
			select {
			case events <- received{event, err}:
			case <-watchCtx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	_, err = n.client.CreateMachine(ctx, createRequest("watched", pulled.Digest))
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "watched"})
	require.NoError(t, err)
	_, err = n.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "watched", GracePeriod: durationpb.New(10 * time.Second)})
	require.NoError(t, err)

	// The wait is for the event the assertions end on, the Machine's stop, which is the last
	// change the test made.
	var states []v1alpha2.MachineState
	var last uint64
	timeout := time.After(time.Minute)
	for len(states) == 0 || states[len(states)-1] != v1alpha2.MachineState_MACHINE_STATE_STOPPED {
		select {
		case r := <-events:
			require.NoError(t, r.err, "the events so far: %v", states)
			require.Greater(t, r.event.Sequence, last, "a sequence number is above the one before it")
			last = r.event.Sequence
			if changed := r.event.GetStateChanged(); changed != nil && r.event.MachineId == "watched" {
				states = append(states, changed.State)
			}
		case <-timeout:
			t.Fatalf("no Stopped event for the Machine a minute after it was stopped; the events so far: %v", states)
		}
	}

	assert.Equal(t, []v1alpha2.MachineState{
		v1alpha2.MachineState_MACHINE_STATE_CREATED,
		v1alpha2.MachineState_MACHINE_STATE_RUNNING,
		v1alpha2.MachineState_MACHINE_STATE_STOPPED,
	}, states)
}

// runMachine creates a Machine and starts it, and returns once it is Running.
func (n *node) runMachine(t *testing.T, ctx context.Context, req *v1alpha2.CreateMachineRequest) {
	t.Helper()
	_, err := n.client.CreateMachine(ctx, req)
	require.NoError(t, err)
	started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: req.MachineId})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)
}

// listed is what List answers: each Machine's state by its identifier.
func (n *node) listed(t *testing.T, ctx context.Context) map[string]v1alpha2.MachineState {
	t.Helper()
	listed, err := n.client.ListMachines(ctx, &v1alpha2.ListMachinesRequest{})
	require.NoError(t, err)
	states := map[string]v1alpha2.MachineState{}
	for _, m := range listed.Machines {
		states[m.Id] = m.State
	}
	return states
}
