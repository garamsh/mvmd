package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// ADR 0040: the Driver states the most of a Machine it accepts, and the
// lifecycle refuses a figure above it as invalid before anything is made.

const limitVCPUs = 4

func limited(caps ...machine.Capability) *env {
	e := newEnv(caps...)
	e.driver = memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: caps, Limits: machine.DriverLimits{MaxVCPUs: limitVCPUs}})
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels()))
	return e
}

func TestCreate_RefusesResourcesOrACeilingAboveTheDriversLimitAsInvalid(t *testing.T) {
	e := limited(hotplugAndResize...)
	atLimit := machine.Resources{VCPUs: limitVCPUs, MemoryBytes: 512 << 20}
	over := machine.Resources{VCPUs: limitVCPUs + 1, MemoryBytes: 512 << 20}

	requireKind(t, createWithCeiling(e, "r", over, over), machine.ErrInvalid)
	requireKind(t, createWithCeiling(e, "c", small, over), machine.ErrInvalid)
	e.requireNoMachine(t, "r")
	e.requireNoMachine(t, "c")

	require.NoError(t, createWithCeiling(e, "ok", atLimit, atLimit), "control: the limit itself")
	require.NoError(t, createWithCeiling(e, "ok2", small, atLimit), "control: a ceiling at the limit")
}

func TestSetResources_RefusesAboveTheDriversLimitAsInvalid(t *testing.T) {
	ctx := context.Background()
	over := machine.Resources{VCPUs: limitVCPUs + 1, MemoryBytes: 512 << 20}
	for _, state := range []machine.State{machine.StateCreated, machine.StateRunning} {
		e := limited(hotplugAndResize...)
		e.ceiling = machine.Resources{VCPUs: limitVCPUs, MemoryBytes: 512 << 20}
		e.inState(t, "m", state)
		requireKind(t, e.svc.SetResources(ctx, "m", over, over), machine.ErrInvalid)
		requireKind(t, e.svc.SetResources(ctx, "m", small, over), machine.ErrInvalid)
		assert.Zero(t, e.driver.Calls("resize"))
		assert.Equal(t, small, e.requireState(t, "m", state).Resources)

		atLimit := machine.Resources{VCPUs: limitVCPUs, MemoryBytes: 512 << 20}
		require.NoError(t, e.svc.SetResources(ctx, "m", atLimit, machine.Resources{}), "control: the limit itself, in %s", state)
	}
}

func TestRestoreSnapshot_RefusesASnapshotAboveTheDriversLimitAsAFailedPrecondition(t *testing.T) {
	ctx := context.Background()
	caps := append(snapshotCaps, machine.CapabilityResize)
	e := newEnv(caps...)
	e.ceiling = machine.Resources{VCPUs: 8, MemoryBytes: 512 << 20}
	e.snapshotted(t, "src", machine.StateRunning)
	info := func(most uint32) machine.DriverInfo {
		return machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: caps, Limits: machine.DriverLimits{MaxVCPUs: most}}
	}

	o := e.elsewhere(info(4))
	o.images.Provide(image)
	requireKind(t, o.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), machine.ErrPreconditionFailed)
	o.requireNoMachine(t, "new")
	assert.Zero(t, o.driver.Calls("describe-restore"), "nothing was begun")

	o = e.elsewhere(info(8))
	o.images.Provide(image)
	require.NoError(t, o.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), "control: a limit at the snapshot's ceiling")
}
