package machine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ADR 0029: a Machine's resource ceiling is chosen at create and bounds a live
// resize. Each refusal below is paired with a control that is the nearest
// value accepted, so a refusal that came from anything else shows.

var (
	small = machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20}
	large = machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}
)

func createWithCeiling(e *env, id machine.MachineID, resources, ceiling machine.Resources) error {
	spec := baseSpec()
	spec.Resources = resources
	spec.Ceiling = ceiling
	return e.svc.Create(context.Background(), id, request(id), spec)
}

func TestCreate_DefaultsTheCeilingFigureByFigure(t *testing.T) {
	cases := []struct {
		name    string
		ceiling machine.Resources
		want    machine.Resources
	}{
		{"unset is the resources", machine.Resources{}, small},
		{"a figure unset is the resources' figure", machine.Resources{MemoryBytes: 1 << 30}, machine.Resources{VCPUs: 2, MemoryBytes: 1 << 30}},
		{"a vCPU figure alone", machine.Resources{VCPUs: 8}, machine.Resources{VCPUs: 8, MemoryBytes: 512 << 20}},
		{"both set are kept", large, large},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(hotplugAndResize...)
			require.NoError(t, createWithCeiling(e, "m", small, c.ceiling))
			assert.Equal(t, c.want, e.requireState(t, "m", machine.StateCreated).Ceiling)
		})
	}
}

func TestCreate_RefusesAResourcesAboveTheCeilingAsInvalid(t *testing.T) {
	e := newEnv(hotplugAndResize...)
	requireKind(t, createWithCeiling(e, "m", large, small), machine.ErrInvalid)
	requireKind(t, createWithCeiling(e, "m2", machine.Resources{VCPUs: 1, MemoryBytes: 1 << 30}, machine.Resources{VCPUs: 1, MemoryBytes: 512 << 20}), machine.ErrInvalid)
	e.requireNoMachine(t, "m")

	require.NoError(t, createWithCeiling(e, "ok", large, large), "control: a ceiling equal to the resources")
}

func TestCreate_RefusesMemoryOffTheAlignmentAsInvalid(t *testing.T) {
	e := newEnv(hotplugAndResize...)
	off := machine.Resources{VCPUs: 2, MemoryBytes: 512<<20 + 1<<20}
	requireKind(t, createWithCeiling(e, "a", off, machine.Resources{}), machine.ErrInvalid)
	requireKind(t, createWithCeiling(e, "b", small, machine.Resources{VCPUs: 2, MemoryBytes: 1<<30 + 4096}), machine.ErrInvalid)
	requireKind(t, createWithCeiling(e, "c", small, machine.Resources{VCPUs: 2, MemoryBytes: 1<<30 + 1}), machine.ErrInvalid)

	on := machine.Resources{VCPUs: 2, MemoryBytes: 514 << 20}
	require.NoError(t, createWithCeiling(e, "d", on, machine.Resources{VCPUs: 2, MemoryBytes: 1<<30 + 2<<20}), "control: a multiple of 2 MiB")
}

func TestCreate_RefusesACeilingAboveTheResourcesWithoutResizeAsUnsupported(t *testing.T) {
	e := newEnv()
	requireKind(t, createWithCeiling(e, "m", small, large), machine.ErrUnsupported)
	e.requireNoMachine(t, "m")

	require.NoError(t, createWithCeiling(e, "same", small, small), "control: a ceiling that is the resources")
	require.NoError(t, createWithCeiling(e, "none", small, machine.Resources{}), "control: no ceiling")
	require.NoError(t, createWithCeiling(newEnv(machine.CapabilityResize), "m", small, large), "control: the Driver can resize")
}

func TestSetResources_RefusesAboveTheCeilingAsInvalid(t *testing.T) {
	ctx := context.Background()
	for _, state := range []machine.State{machine.StateCreated, machine.StateRunning} {
		e := newEnv(hotplugAndResize...)
		e.ceiling = large
		e.inState(t, "m", state)
		above := machine.Resources{VCPUs: 5, MemoryBytes: 512 << 20}
		requireKind(t, e.svc.SetResources(ctx, "m", above, machine.Resources{}), machine.ErrInvalid)
		requireKind(t, e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 2, MemoryBytes: 1<<30 + 2<<20}, machine.Resources{}), machine.ErrInvalid)
		assert.Equal(t, small, e.requireState(t, "m", state).Resources)

		require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}), "control: the ceiling itself, in %s", state)
	}
}

func TestSetResources_RefusesMemoryOffTheAlignmentAsInvalid(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)
	requireKind(t, e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 2, MemoryBytes: 600<<20 + 1}, machine.Resources{}), machine.ErrInvalid)
	assert.Zero(t, e.driver.Calls("resize"))

	require.NoError(t, e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 2, MemoryBytes: 600 << 20}, machine.Resources{}), "control: a multiple of 2 MiB")
}

func TestSetResources_ACeilingChangeNeedsAMachineNotRunningOrPaused(t *testing.T) {
	ctx := context.Background()
	for _, state := range []machine.State{machine.StateRunning, machine.StatePaused} {
		e := newEnv(hotplugAndResize...)
		e.ceiling = large
		e.inState(t, "m", state)
		bigger := machine.Resources{VCPUs: 8, MemoryBytes: 2 << 30}
		requireKind(t, e.svc.SetResources(ctx, "m", small, bigger), machine.ErrPreconditionFailed)
		assert.Equal(t, large, e.requireState(t, "m", state).Ceiling)

		require.NoError(t, e.svc.SetResources(ctx, "m", large, large), "control: the ceiling it has, in %s", state)
	}

	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateStopped)
	require.NoError(t, e.svc.SetResources(ctx, "m", small, machine.Resources{VCPUs: 8, MemoryBytes: 2 << 30}), "control: a Stopped Machine")
	assert.Equal(t, machine.Resources{VCPUs: 8, MemoryBytes: 2 << 30}, e.requireState(t, "m", machine.StateStopped).Ceiling)
}

func TestSetResources_ACeilingAboveTheResourcesNeedsResize(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	e.created(t, "m")
	requireKind(t, e.svc.SetResources(ctx, "m", small, large), machine.ErrUnsupported)
	require.NoError(t, e.svc.SetResources(ctx, "m", small, small), "control: a ceiling that is the resources")

	with := newEnv(machine.CapabilityResize)
	with.created(t, "m")
	require.NoError(t, with.svc.SetResources(ctx, "m", small, large), "control: the Driver can resize")
}

func TestSetResources_AHotTargetBelowTheBootMemoryNeedsTheMachineStopped(t *testing.T) {
	ctx := context.Background()
	for _, state := range []machine.State{machine.StateRunning, machine.StatePaused} {
		e := newEnv(hotplugAndResize...)
		e.ceiling = large
		e.inState(t, "m", state)
		below := machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}
		err := e.svc.SetResources(ctx, "m", below, machine.Resources{})
		requireKind(t, err, machine.ErrPreconditionFailed)
		assert.Contains(t, strings.ToLower(err.Error()), "stop the machine", "the reason names what applies it")
		assert.Zero(t, e.driver.Calls("resize"))
		assert.Equal(t, small, e.requireState(t, "m", state).Resources)

		at := machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20}
		require.NoError(t, e.svc.SetResources(ctx, "m", at, machine.Resources{}), "control: the boot memory, in %s", state)
	}

	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateStopped)
	require.NoError(t, e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}, machine.Resources{}), "control: a Stopped Machine")
	require.NoError(t, e.svc.Start(ctx, "m"))
	assert.Equal(t, uint64(256<<20), e.requireState(t, "m", machine.StateRunning).BootMemoryBytes, "it boots with what the target gave")
}

func TestSetResources_AnOmittedCeilingFigureLeavesTheCeiling(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateStopped)
	require.NoError(t, e.svc.SetResources(ctx, "m", small, machine.Resources{VCPUs: 8}))
	assert.Equal(t, machine.Resources{VCPUs: 8, MemoryBytes: 1 << 30}, e.requireState(t, "m", machine.StateStopped).Ceiling)

	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}))
	assert.Equal(t, machine.Resources{VCPUs: 8, MemoryBytes: 1 << 30}, e.requireState(t, "m", machine.StateStopped).Ceiling, "none given: unchanged")
}

func TestStart_RecordsTheMemoryTheVMMBootedWith(t *testing.T) {
	e := newEnv(hotplugAndResize...)
	e.ceiling = large
	e.inState(t, "m", machine.StateRunning)
	got := e.requireState(t, "m", machine.StateRunning)
	assert.Equal(t, small.MemoryBytes, got.BootMemoryBytes)
	assert.Equal(t, large, e.driver.BootSpec("m").Ceiling, "the Driver is told the ceiling")
	assert.Equal(t, small, e.driver.BootSpec("m").Resources)
}

func TestSnapshot_CarriesTheCeilingAndTheBootMemoryAndARestoreUsesThem(t *testing.T) {
	ctx := context.Background()
	e := newEnv(append(snapshotCaps, machine.CapabilityResize)...)
	e.ceiling = large
	e.snapshotted(t, "m", machine.StateRunning)

	got, _, err := e.snaps.Get(ctx, "snap")
	require.NoError(t, err)
	assert.Equal(t, large, got.Spec.Ceiling)
	assert.Equal(t, small.MemoryBytes, got.Spec.BootMemoryBytes)

	require.NoError(t, e.svc.RestoreSnapshot(ctx, "copy", "r-copy", "snap", machine.NetworkPolicy{}))
	restored := e.requireState(t, "copy", machine.StateRunning)
	assert.Equal(t, large, restored.Ceiling)
	assert.Equal(t, small.MemoryBytes, restored.BootMemoryBytes)
	assert.Equal(t, large, e.driver.BootSpec("copy").Ceiling)
}

// On a Driver that cannot resize the ceiling is the resources, so it follows a
// cold target in either direction and never loosens the VMM's cgroup; on one
// that can, the held ceiling is kept (ADR 0029).
func TestSetResources_WithoutResizeTheCeilingFollowsTheTarget(t *testing.T) {
	ctx := context.Background()
	less := machine.Resources{VCPUs: 1, MemoryBytes: 256 << 20}

	e := newEnv()
	e.created(t, "m")
	require.NoError(t, e.svc.SetResources(ctx, "m", less, machine.Resources{}))
	got := e.requireState(t, "m", machine.StateCreated)
	assert.Equal(t, less, got.Resources)
	assert.Equal(t, less, got.Ceiling, "a decrease lowers the ceiling with it")

	require.NoError(t, e.svc.SetResources(ctx, "m", large, machine.Resources{}))
	got = e.requireState(t, "m", machine.StateCreated)
	assert.Equal(t, large, got.Resources)
	assert.Equal(t, large, got.Ceiling, "an increase needs no ceiling and raises it")

	requireKind(t, e.svc.SetResources(ctx, "m", less, large), machine.ErrUnsupported)

	with := newEnv(machine.CapabilityResize)
	with.ceiling = large
	with.created(t, "m")
	require.NoError(t, with.svc.SetResources(ctx, "m", less, machine.Resources{}))
	got = with.requireState(t, "m", machine.StateCreated)
	assert.Equal(t, less, got.Resources, "control: a Driver that resizes")
	assert.Equal(t, large, got.Ceiling, "keeps the ceiling it holds")
}
