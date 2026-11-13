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

// osServer is a runtime API server over the real Machine use case whose node
// offers the memory Driver's boot set "os".
func osServer() *api.Server {
	info := machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu"}
	svc := machine.NewService(memory.NewDriver(info), memory.NewSupervision(), memory.NewNetwork(), memory.NewDisk(), memory.NewImageStore(), memory.NewSnapshotStore(),
		memory.NewGuest(0), memory.NewWallClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)), memory.NewStateStore(), memory.NewStartPermitter(),
		machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), memory.NewEvents(), machine.WithKernels(memory.Kernels()), machine.WithBootSets(memory.BootSets(), &memory.BootFiles{}))
	return api.New(svc, nil, nil, info, capacitymemory.NewReader(nodecapacity.Capacity{}), nil, api.WithBootSets(memory.BootSets()))
}

func osRequest(id string) *v1alpha2.CreateMachineRequest {
	return &v1alpha2.CreateMachineRequest{
		MachineId: id, RequestId: "r-" + id,
		OsBoot:    &v1alpha2.OsBoot{BootSet: "os", KernelCmdline: "console=ttyS0"},
		Resources: &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
	}
}

func TestGetRuntimeInfo_ListsTheBootSetsByName(t *testing.T) {
	resp, err := osServer().GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetBootSets(), 1)
	assert.Equal(t, "os", resp.GetBootSets()[0].GetName())
	assert.Equal(t, memory.BootSets()["os"].Kernel.SHA256, resp.GetBootSets()[0].GetKernelDigest())
	assert.Equal(t, memory.BootSets()["os"].Initrd.SHA256, resp.GetBootSets()[0].GetInitrdDigest())

	bare, err := api.New(nil, nil, nil, machine.DriverInfo{}, capacitymemory.NewReader(nodecapacity.Capacity{}), nil).GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	assert.Empty(t, bare.GetBootSets(), "control: a node with none lists none")
}

func TestCreateMachine_OSForm(t *testing.T) {
	ctx := context.Background()
	srv := osServer()

	created, err := srv.CreateMachine(ctx, osRequest("m"))
	require.NoError(t, err)
	assert.Equal(t, "os", created.GetMachine().GetOsBoot().GetBootSet())
	assert.Equal(t, "console=ttyS0", created.GetMachine().GetOsBoot().GetKernelCmdline())
	assert.Empty(t, created.GetMachine().GetImageDigest())

	got, err := srv.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "m"})
	require.NoError(t, err)
	assert.Equal(t, "os", got.GetMachine().GetOsBoot().GetBootSet(), "Inspect reports the form and boot set")

	withImage := osRequest("i")
	withImage.ImageDigest = "sha256:x"
	_, err = srv.CreateMachine(ctx, withImage)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.ErrorContains(t, err, "image_digest")
}
