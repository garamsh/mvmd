package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	capacitymemory "github.com/garamsh/mvmd/daemon/internal/nodecapacity/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const limitImage machine.ImageDigest = "sha256:limit"

var limitCaps = []machine.Capability{machine.CapabilitySnapshot, machine.CapabilityCPUResize, machine.CapabilityMemoryResize}

// limitedServer is a runtime API server over the real Machine use case and the
// in-memory adapters, whose Driver accepts most vCPUs. Servers made over one
// snapshot store share its snapshots.
func limitedServer(snaps *memory.SnapshotStore, most uint32) (*api.Server, machine.Service) {
	info := machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu", Capabilities: limitCaps, Limits: machine.DriverLimits{MaxVCPUs: most}}
	images := memory.NewImageStore()
	images.Provide(limitImage)
	svc := machine.NewService(memory.NewDriver(info), memory.NewSupervision(), memory.NewNetwork(), memory.NewDisk(), images, snaps,
		memory.NewGuest(0), memory.NewWallClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)), memory.NewStateStore(), memory.NewStartPermitter(),
		machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), memory.NewEvents(), machine.WithKernels(memory.Kernels()))
	return api.New(svc, nil, nil, info, capacitymemory.NewReader(nodecapacity.Capacity{}), nil), svc
}

func createRequest(id string, vcpus, ceilingVCPUs uint32) *v1alpha2.CreateMachineRequest {
	req := &v1alpha2.CreateMachineRequest{
		MachineId: id, RequestId: "r-" + id, ImageDigest: string(limitImage),
		Resources:     &v1alpha2.Resources{CpuCount: vcpus, MemoryBytes: 512 << 20},
		RootDiskBytes: 1 << 30,
	}
	if ceilingVCPUs != 0 {
		req.ResourceCeiling = &v1alpha2.Resources{CpuCount: ceilingVCPUs}
	}
	return req
}

func TestGetRuntimeInfo_ReportsTheDriversLimitsOrNoneWhenItStatesNone(t *testing.T) {
	limited := api.New(nil, nil, nil, machine.DriverInfo{Name: "a", Version: "1", Limits: machine.DriverLimits{MaxVCPUs: 32}}, capacitymemory.NewReader(nodecapacity.Capacity{}), nil)
	resp, err := limited.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetDrivers(), 1)
	assert.Equal(t, uint32(32), resp.GetDrivers()[0].GetLimits().GetMaxVcpus())

	unlimited := api.New(nil, nil, nil, machine.DriverInfo{Name: "b", Version: "1"}, capacitymemory.NewReader(nodecapacity.Capacity{}), nil)
	resp, err = unlimited.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetDrivers(), 1)
	assert.Nil(t, resp.GetDrivers()[0].GetLimits(), "a Driver that states none reports none, not zero")
}

func TestCreateMachine_RefusesAboveTheReportedLimitAsRejectedAndCreatesNothing(t *testing.T) {
	ctx := context.Background()
	srv, svc := limitedServer(memory.NewSnapshotStore(), 4)
	info, err := srv.GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	most := info.GetDrivers()[0].GetLimits().GetMaxVcpus()
	require.Equal(t, uint32(4), most, "the setup: the limit is reported")

	_, err = srv.CreateMachine(ctx, createRequest("over", most+1, 0))
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	_, err = srv.CreateMachine(ctx, createRequest("ceil", 1, most+1))
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	all, err := svc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, all, "no Machine was made")

	_, err = srv.CreateMachine(ctx, createRequest("at", most, most))
	require.NoError(t, err, "control: the reported limit is accepted")
}

func TestSetResources_RefusesAboveTheReportedLimitAsRejectedAndChangesNothing(t *testing.T) {
	ctx := context.Background()
	srv, _ := limitedServer(memory.NewSnapshotStore(), 4)
	_, err := srv.CreateMachine(ctx, createRequest("m", 2, 4))
	require.NoError(t, err)

	_, err = srv.SetResources(ctx, &v1alpha2.SetResourcesRequest{MachineId: "m", Target: &v1alpha2.Resources{CpuCount: 5, MemoryBytes: 512 << 20}, ResourceCeiling: &v1alpha2.Resources{CpuCount: 5}})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	got, err := srv.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, uint32(2), got.GetMachine().GetResources().GetCpuCount())

	_, err = srv.SetResources(ctx, &v1alpha2.SetResourcesRequest{MachineId: "m", Target: &v1alpha2.Resources{CpuCount: 4, MemoryBytes: 512 << 20}})
	require.NoError(t, err, "control: the reported limit is accepted")
}

func TestRestoreMachine_RefusesASnapshotAboveTheReportedLimitAsPreconditionFailedAndRestoresNothing(t *testing.T) {
	ctx := context.Background()
	snaps := memory.NewSnapshotStore()
	big, bigSvc := limitedServer(snaps, 8)
	_, err := big.CreateMachine(ctx, createRequest("src", 2, 8))
	require.NoError(t, err)
	require.NoError(t, bigSvc.Start(ctx, "src"))
	require.NoError(t, bigSvc.Snapshot(ctx, "src", "snap", "r-snap"))

	small, smallSvc := limitedServer(snaps, 4)
	_, err = small.RestoreMachine(ctx, &v1alpha2.RestoreMachineRequest{MachineId: "copy", RequestId: "r-copy", SnapshotId: "snap"})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err))
	all, err := smallSvc.List(ctx)
	require.NoError(t, err)
	assert.Empty(t, all, "no Machine was restored")

	same, _ := limitedServer(snaps, 8)
	_, err = same.RestoreMachine(ctx, &v1alpha2.RestoreMachineRequest{MachineId: "copy", RequestId: "r-copy", SnapshotId: "snap"})
	require.NoError(t, err, "control: a Driver whose limit holds the snapshot's ceiling restores it")
}
