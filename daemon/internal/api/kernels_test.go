package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	capacitymemory "github.com/garamsh/mvmd/daemon/internal/nodecapacity/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func TestGetRuntimeInfo_ListsTheKernelsByNameWithTheDefaultMarked(t *testing.T) {
	k := machine.Kernels{Default: "stable", Offered: map[string]machine.KernelFile{
		"stable": {Path: "/k/a", SHA256: "digest-a"},
		"next":   {Path: "/k/b", SHA256: "digest-b"},
	}}

	resp, err := api.New(nil, nil, nil, machine.DriverInfo{}, capacitymemory.NewReader(nodecapacity.Capacity{}), nil, api.WithKernels(k)).GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})

	require.NoError(t, err)
	require.Len(t, resp.GetKernels(), 2)
	assert.Equal(t, "next", resp.GetKernels()[0].GetName())
	assert.Equal(t, "digest-b", resp.GetKernels()[0].GetDigest())
	assert.False(t, resp.GetKernels()[0].GetIsDefault())
	assert.Equal(t, "stable", resp.GetKernels()[1].GetName())
	assert.Equal(t, "digest-a", resp.GetKernels()[1].GetDigest())
	assert.True(t, resp.GetKernels()[1].GetIsDefault())
}

func TestListMachines_ReportsTheKernelNameOrNone(t *testing.T) {
	m := &lister{all: []machine.Machine{{ID: "named", Spec: machine.Spec{Kernel: "next"}}, {ID: "legacy"}}}

	resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).ListMachines(context.Background(), &v1alpha2.ListMachinesRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Machines, 2)
	assert.Equal(t, "next", resp.Machines[0].Kernel)
	assert.Empty(t, resp.Machines[1].Kernel)
}

func TestListSnapshots_ReportsTheKernelName(t *testing.T) {
	m := &snapshotter{listed: []machine.Snapshot{{ID: "snap", Compat: machine.SnapshotCompat{KernelName: "next"}}}}

	resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).ListSnapshots(context.Background(), &v1alpha2.ListSnapshotsRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Snapshots, 1)
	assert.Equal(t, "next", resp.Snapshots[0].Kernel)
}

func TestCreateMachine_PassesTheKernelNameThrough(t *testing.T) {
	ctx := context.Background()
	srv, _ := limitedServer(memory.NewSnapshotStore(), 4)

	_, err := srv.CreateMachine(ctx, createRequest("dflt", 1, 0))
	require.NoError(t, err)
	got, err := srv.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "dflt"})
	require.NoError(t, err)
	assert.Equal(t, "default", got.GetMachine().GetKernel(), "no name records the node's default")

	req := createRequest("named", 1, 0)
	req.Kernel = "default"
	_, err = srv.CreateMachine(ctx, req)
	require.NoError(t, err, "control: an offered name is accepted")
}

func TestCreateMachine_RefusesAKernelTheNodeDoesNotOfferAsRejected(t *testing.T) {
	ctx := context.Background()
	srv, svc := limitedServer(memory.NewSnapshotStore(), 4)

	req := createRequest("m", 1, 0)
	req.Kernel = "nope"
	_, err := srv.CreateMachine(ctx, req)

	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	all, err := svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, all, "no Machine was made")
}
