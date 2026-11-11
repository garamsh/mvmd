package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// A Driver may resize the vCPUs, the memory, both or neither. A call that
// changes a resource the Driver cannot resize is unsupported and changes
// nothing, whatever it changes besides.

var (
	cpuOnly    = []machine.Capability{machine.CapabilityCPUResize}
	memoryOnly = []machine.Capability{machine.CapabilityMemoryResize}
	both       = []machine.Capability{machine.CapabilityCPUResize, machine.CapabilityMemoryResize}
)

// A ceiling above the resources is only held in a resource the Driver can
// resize, so each case names the ceiling its Driver accepts at create. A target
// that stays inside it changes a resource the Driver cannot resize by going
// down, since going up would be above the ceiling and invalid.
func TestSetResources_RefusesAResourceWithoutItsCapabilityAsUnsupportedAndChangesNothing(t *testing.T) {
	cases := []struct {
		name     string
		caps     []machine.Capability
		ceiling  machine.Resources
		target   machine.Resources
		wantDone bool
	}{
		{"CPU only Driver, vCPUs changed", cpuOnly, machine.Resources{VCPUs: 4}, machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20}, true},
		{"CPU only Driver, memory changed", cpuOnly, machine.Resources{VCPUs: 4}, machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}, false},
		{"CPU only Driver, both changed", cpuOnly, machine.Resources{VCPUs: 4}, machine.Resources{VCPUs: 4, MemoryBytes: 256 << 20}, false},
		{"memory only Driver, memory changed", memoryOnly, machine.Resources{MemoryBytes: 1 << 30}, machine.Resources{VCPUs: 2, MemoryBytes: 1 << 30}, true},
		{"memory only Driver, vCPUs changed", memoryOnly, machine.Resources{MemoryBytes: 1 << 30}, machine.Resources{VCPUs: 1, MemoryBytes: 512 << 20}, false},
		{"memory only Driver, both changed", memoryOnly, machine.Resources{MemoryBytes: 1 << 30}, machine.Resources{VCPUs: 1, MemoryBytes: 1 << 30}, false},
		{"both capabilities, both changed", both, large, large, true},
		{"no capability, vCPUs changed", nil, machine.Resources{}, machine.Resources{VCPUs: 1, MemoryBytes: 512 << 20}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(c.caps...)
			e.ceiling = c.ceiling
			e.inState(t, "m", machine.StateRunning)
			before := e.requireState(t, "m", machine.StateRunning)
			require.LessOrEqual(t, c.target.VCPUs, before.Ceiling.VCPUs, "the setup: the target is inside the ceiling")
			require.LessOrEqual(t, c.target.MemoryBytes, before.Ceiling.MemoryBytes, "the setup: the target is inside the ceiling")

			err := e.svc.SetResources(ctx, "m", c.target, machine.Resources{})
			if c.wantDone {
				require.NoError(t, err)
				assert.Equal(t, c.target, e.requireState(t, "m", machine.StateRunning).Resources)
				assert.Equal(t, 1, e.driver.Calls("resize"))
				return
			}
			requireKind(t, err, machine.ErrUnsupported)
			assert.Equal(t, before.Resources, e.requireState(t, "m", machine.StateRunning).Resources)
			assert.Zero(t, e.driver.Calls("resize"), "the Driver was not asked")
		})
	}
}

func TestCreate_RefusesACeilingAboveTheResourcesInAResourceWithoutItsCapabilityAsUnsupported(t *testing.T) {
	cases := []struct {
		name    string
		caps    []machine.Capability
		ceiling machine.Resources
		refused bool
	}{
		{"CPU only Driver, ceiling above in vCPUs", cpuOnly, machine.Resources{VCPUs: 4}, false},
		{"CPU only Driver, ceiling above in memory", cpuOnly, machine.Resources{MemoryBytes: 1 << 30}, true},
		{"memory only Driver, ceiling above in memory", memoryOnly, machine.Resources{MemoryBytes: 1 << 30}, false},
		{"memory only Driver, ceiling above in vCPUs", memoryOnly, machine.Resources{VCPUs: 4}, true},
		{"memory only Driver, ceiling above in both", memoryOnly, large, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(c.caps...)
			err := createWithCeiling(e, "m", small, c.ceiling)
			if c.refused {
				requireKind(t, err, machine.ErrUnsupported)
				e.requireNoMachine(t, "m")
				return
			}
			require.NoError(t, err)
			assert.Equal(t, c.ceiling.OrElse(small).VCPUs, e.requireState(t, "m", machine.StateCreated).Ceiling.VCPUs)
		})
	}
}

func TestSetResources_AStoppedMachineHoldsTheCeilingOfAResourceTheDriverCannotResizeAtTheTarget(t *testing.T) {
	ctx := context.Background()
	e := newEnv(cpuOnly...)
	e.ceiling = machine.Resources{VCPUs: 4, MemoryBytes: 512 << 20}
	e.inState(t, "m", machine.StateStopped)

	require.NoError(t, e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 3, MemoryBytes: 1 << 30}, machine.Resources{}))
	got := e.requireState(t, "m", machine.StateStopped)
	assert.Equal(t, machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}, got.Ceiling, "the vCPU ceiling is kept, the memory ceiling follows the target")
}
