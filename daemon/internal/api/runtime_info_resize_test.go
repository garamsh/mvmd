package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	capacitymemory "github.com/garamsh/mvmd/daemon/internal/nodecapacity/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func TestGetRuntimeInfo_ReportsEachResizeCapabilityAsItsOwnWireValue(t *testing.T) {
	assert.Equal(t, []v1alpha2.Capability{v1alpha2.Capability_CAPABILITY_CPU_RESIZE},
		runtimeInfo(t, machine.CapabilityCPUResize))
	assert.Equal(t, []v1alpha2.Capability{v1alpha2.Capability_CAPABILITY_MEMORY_RESIZE},
		runtimeInfo(t, machine.CapabilityMemoryResize))
	assert.ElementsMatch(t, []v1alpha2.Capability{v1alpha2.Capability_CAPABILITY_CPU_RESIZE, v1alpha2.Capability_CAPABILITY_MEMORY_RESIZE},
		runtimeInfo(t, machine.CapabilityCPUResize, machine.CapabilityMemoryResize))
	assert.Empty(t, runtimeInfo(t), "control: a Driver with neither reports neither")
}

func TestGetRuntimeInfo_ListsTheOneDriverTheNodeServes(t *testing.T) {
	srv := api.New(nil, nil, nil, []machine.DriverInfo{{Name: "only", Version: "1"}}, capacitymemory.NewReader(nodecapacity.Capacity{}), nil)
	resp, err := srv.GetRuntimeInfo(context.Background(), &v1alpha2.GetRuntimeInfoRequest{})
	require.NoError(t, err)
	require.Len(t, resp.GetDrivers(), 1)
	assert.Equal(t, "only", resp.GetDrivers()[0].GetDriver().GetName())
}
