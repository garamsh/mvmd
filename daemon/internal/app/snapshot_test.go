package app_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

// Snapshot and Restore through the wired graph: a Running Machine is
// snapshotted and left Running, and a new Machine is restored from it, Running,
// under the policy of the request.
func TestAPI_SnapshotAndRestoreMachine(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)
	renew(t, n, 1)
	c := n.client

	_, err := c.CreateMachine(ctx, create("src"))
	require.NoError(t, err)
	_, err = c.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "src"})
	require.NoError(t, err)

	snap := &v1alpha1.SnapshotMachineRequest{SnapshotId: "snap", RequestId: "r-snap", MachineId: "src"}
	resp, err := c.SnapshotMachine(ctx, snap)
	require.NoError(t, err)
	assert.Equal(t, "snap", resp.GetSnapshotId())
	assert.Equal(t, []machine.SnapshotID{"snap"}, n.app.Substitutes.Snapshots.Held(), "the substitute the graph is wired with holds it")
	got, err := c.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "src"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, got.GetMachine().GetState(), "the Machine is left as it was")

	_, err = c.SnapshotMachine(ctx, snap)
	require.NoError(t, err, "the same request ID: the existing snapshot")
	_, err = c.SnapshotMachine(ctx, &v1alpha1.SnapshotMachineRequest{SnapshotId: "snap", RequestId: "other", MachineId: "src"})
	requireKind(t, err, conflict)

	policy := &v1alpha1.NetworkPolicy{Egress: []*v1alpha1.NetworkRule{{
		Prefix:   "192.168.0.0/16",
		Protocol: v1alpha1.Protocol_PROTOCOL_TCP,
		Ports:    []*v1alpha1.PortRange{{First: 22, Last: 22}},
	}}}
	restored, err := c.RestoreMachine(ctx, &v1alpha1.RestoreMachineRequest{MachineId: "new", RequestId: "r-new", SnapshotId: "snap", NetworkPolicy: policy})
	require.NoError(t, err)
	m := restored.GetMachine()
	assert.Equal(t, "new", m.GetId())
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, m.GetState())
	assert.Equal(t, "192.168.0.0/16", m.GetNetworkPolicy().GetEgress()[0].GetPrefix(), "the request's policy, not the source's")
	assert.NotEqual(t, got.GetMachine().GetAddresses(), m.GetAddresses(), "new addresses")

	list, err := c.ListMachines(ctx, &v1alpha1.ListMachinesRequest{})
	require.NoError(t, err)
	assert.Len(t, list.GetMachines(), 2)
}

func TestAPI_SnapshotAndRestoreAnswerEveryOutcome(t *testing.T) {
	ctx := context.Background()
	n := start(t, testConfig(t))
	n.app.Substitutes.Images.Provide(digest)
	c := n.client
	_, err := c.CreateMachine(ctx, create("created"))
	require.NoError(t, err)

	_, err = c.SnapshotMachine(ctx, &v1alpha1.SnapshotMachineRequest{SnapshotId: "s", RequestId: "r", MachineId: "created"})
	requireKind(t, err, precondition) // Created
	_, err = c.SnapshotMachine(ctx, &v1alpha1.SnapshotMachineRequest{SnapshotId: "s", RequestId: "r", MachineId: "nobody"})
	requireKind(t, err, notFound)
	_, err = c.SnapshotMachine(ctx, &v1alpha1.SnapshotMachineRequest{SnapshotId: "s", RequestId: "r"})
	requireKind(t, err, rejected) // no machine_id
	_, err = c.RestoreMachine(ctx, &v1alpha1.RestoreMachineRequest{MachineId: "new", RequestId: "r", SnapshotId: "nope"})
	requireKind(t, err, notFound)
	_, err = c.RestoreMachine(ctx, &v1alpha1.RestoreMachineRequest{MachineId: "new", RequestId: "r"})
	requireKind(t, err, rejected) // no snapshot_id
}
