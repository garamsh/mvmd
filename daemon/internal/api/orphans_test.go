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

// Runtime info lists the supervisors the node found at its start under an identifier no record
// holds, ordered, and none when there is none (ADR 0071).
func TestGetRuntimeInfo_ListsTheOrphanedSupervisors(t *testing.T) {
	ctx := context.Background()
	sup := memory.NewSupervision()
	for _, id := range []machine.MachineID{"zeta", "alpha"} {
		require.NoError(t, sup.Start(ctx, id, machine.ProcessDescription{Executable: "/vmm"}, machine.User{UID: 2000, GID: 2000}))
	}
	info := machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu"}
	build := func(sup *memory.Supervision) *api.Server {
		svc := machine.NewService(memory.NewDriver(info), sup, memory.NewNetwork(), memory.NewDisk(), memory.NewImageStore(), memory.NewSnapshotStore(),
			memory.NewGuest(0), memory.NewWallClock(time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)), memory.NewStateStore(), memory.NewStartPermitter(),
			machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), memory.NewEvents(), machine.WithKernels(memory.Kernels()))
		require.NoError(t, svc.Restore(ctx))
		return api.New(svc, nil, nil, info, capacitymemory.NewReader(nodecapacity.Capacity{}), nil)
	}

	resp, err := build(sup).GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	var ids []string
	for _, o := range resp.GetOrphanedSupervisors() {
		ids = append(ids, o.GetMachineId())
	}
	assert.Equal(t, []string{"alpha", "zeta"}, ids)

	resp, err = build(memory.NewSupervision()).GetRuntimeInfo(ctx, &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	assert.Empty(t, resp.GetOrphanedSupervisors(), "control: a node that found none lists none")
}
