package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// Create Machine passes restart_on_boot on and Inspect reports it; a Machine
// created without it reports false (ADR 0062).
func TestCreateMachine_RestartOnBootIsReportedByCreateAndInspect(t *testing.T) {
	ctx := context.Background()
	srv, _ := limitedServer(memory.NewSnapshotStore(), 4)

	req := createRequest("restarting", 1, 0)
	req.RestartOnBoot = true
	resp, err := srv.CreateMachine(ctx, req)
	require.NoError(t, err)
	assert.True(t, resp.GetMachine().GetRestartOnBoot())
	got, err := srv.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "restarting"})
	require.NoError(t, err)
	assert.True(t, got.GetMachine().GetRestartOnBoot(), "Inspect reports it")

	_, err = srv.CreateMachine(ctx, createRequest("plain", 1, 0))
	require.NoError(t, err)
	got, err = srv.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "plain"})
	require.NoError(t, err)
	assert.False(t, got.GetMachine().GetRestartOnBoot())
}
