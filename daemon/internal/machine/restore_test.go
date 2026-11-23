package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// reopened is a second lifecycle over the same store, as a restarted daemon
// builds it, with the substitutes the test chooses to keep or empty.
func (e *env) reopened(t *testing.T) machine.Service {
	t.Helper()
	return e.reopenedWith(t)
}

// reopenedWith is reopened with options.
func (e *env) reopenedWith(t *testing.T, options ...machine.Option) machine.Service {
	t.Helper()
	svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, append([]machine.Option{machine.WithKernels(memory.Kernels()), machine.WithCatalog(e.catalog)}, options...)...)
	require.NoError(t, svc.Restore(context.Background()))
	return svc
}

func TestRestore_RunningAndPausedMachinesWithoutASupervisorAreStopped(t *testing.T) {
	ctx := context.Background()
	for _, state := range []machine.State{machine.StateRunning, machine.StatePaused} {
		t.Run(state.String(), func(t *testing.T) {
			e := newEnv(hotplugAndResize...)
			e.inState(t, "m", state)
			require.NoError(t, e.sup.Terminate(ctx, "m"), "the supervisor and its VMM ended with the daemon's absence")

			m, err := e.reopened(t).Inspect(ctx, "m")
			require.NoError(t, err)
			assert.Equal(t, machine.StateStopped, m.State)
			assert.Contains(t, m.FailureReason, "the VMM ended while mvmd was not running: ")
			_, tap := e.net.Applied("m")
			assert.False(t, tap)
			rec, _ := e.store.Record("m")
			assert.Equal(t, machine.StateStopped, rec.Machine.State, "the finding is recorded")
		})
	}
}

func TestRestore_ALiveSupervisorKeepsTheRecordedState(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.inState(t, "r", machine.StateRunning)
	e.inState(t, "p", machine.StatePaused)

	svc := e.reopened(t)
	for id, want := range map[machine.MachineID]machine.State{"r": machine.StateRunning, "p": machine.StatePaused} {
		m, err := svc.Inspect(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, m.State)
	}
}

func TestRestore_AFailedListingLeavesMachinesThatDoNotClaimToRun(t *testing.T) {
	e := newEnv(hotplugAndResize...)
	e.inState(t, "c", machine.StateCreated)
	e.inState(t, "s", machine.StateStopped)
	e.sup.FailAt("live", errBoom)

	svc := e.reopened(t)
	m, err := svc.Inspect(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, machine.StateCreated, m.State)
}

func TestRestore_ANetworkTheNodeLostFaultsTheMachineAndTheOthersAreKept(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.inState(t, "lost", machine.StateRunning)
	e.inState(t, "paused-lost", machine.StatePaused)
	e.inState(t, "kept", machine.StateRunning)
	e.inState(t, "created", machine.StateCreated)
	// The adapter reports what it cannot restore; a Created Machine has no VMM
	// and holds no network to lose, so naming it changes nothing.
	for _, id := range []machine.MachineID{"lost", "paused-lost", "created"} {
		e.net.LoseAtReconcile(id)
	}

	svc := e.reopened(t)
	for id, want := range map[machine.MachineID]machine.State{
		"lost": machine.StateFaulted, "paused-lost": machine.StateFaulted, "kept": machine.StateRunning, "created": machine.StateCreated,
	} {
		m, err := svc.Inspect(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, m.State, id)
		if want == machine.StateFaulted {
			assert.Contains(t, m.FailureReason, "network could not be restored")
			e.requireStored(t, id, machine.StateFaulted)
		}
	}
	assert.ElementsMatch(t, []machine.MachineID{"lost", "paused-lost", "kept", "created"}, e.net.Reconciled(), "the adapter is given every Machine")
}

func TestRestore_ANetworkThatCannotBeReconciledFailsTheRestore(t *testing.T) {
	e := newEnv(hotplugAndResize...)
	e.inState(t, "m", machine.StateRunning)
	e.net.FailAt("reconcile", errBoom)

	svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels()))
	require.ErrorIs(t, svc.Restore(context.Background()), errBoom)

	e.net.Heal("reconcile")
	e.reopened(t) // control: a restore succeeds once the adapter does
}

var someProcess = machine.ProcessDescription{Executable: "vmm"}
var someUser = machine.User{UID: 1000, GID: 1000}

// A supervisor a record holds but no Running or Paused record claims is ended; the
// controls are the supervisor of a Running record, which is kept, and one no
// record holds at all, which is left running (ADR 0071).
func TestRestore_EndsSupervisorsNoRunningRecordClaims(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.inState(t, "running", machine.StateRunning)
	e.inState(t, "created", machine.StateCreated)
	e.inState(t, "stopped", machine.StateStopped)
	for _, id := range []machine.MachineID{"created", "stopped", "ghost"} {
		require.NoError(t, e.sup.Start(ctx, id, someProcess, someUser))
	}
	_, err := e.net.Apply(ctx, "created", someUser, machine.NetworkPolicy{})
	require.NoError(t, err)

	svc := e.reopened(t)

	_, alive := e.sup.Running("running")
	assert.True(t, alive, "control: the supervisor of a Running record is kept")
	for _, id := range []machine.MachineID{"created", "stopped"} {
		_, alive := e.sup.Running(id)
		assert.False(t, alive, "the supervisor of %q is ended", id)
	}
	_, ghost := e.sup.Running("ghost")
	assert.True(t, ghost, "the supervisor no record holds is left running")
	assert.Equal(t, []machine.MachineID{"ghost"}, svc.Orphans(ctx), "and reported")
	assert.Equal(t, []machine.MachineID{"ghost"}, e.events.OrphanedMachines(), "with an event")
	assert.True(t, e.sup.HasState("ghost"), "its state directory is not swept")
	_, tap := e.net.Applied("created")
	assert.False(t, tap, "the network of a recorded Machine is withdrawn")
	for id, want := range map[machine.MachineID]machine.State{
		"running": machine.StateRunning, "created": machine.StateCreated, "stopped": machine.StateStopped,
	} {
		m, err := svc.Inspect(ctx, id)
		require.NoError(t, err)
		assert.Equal(t, want, m.State, "%q keeps its recorded state", id)
	}
	_, err = svc.Inspect(ctx, "ghost")
	require.ErrorIs(t, err, machine.ErrNotFound, "no Machine appears for a supervisor without a record")
	assert.False(t, e.net.Allocated("ghost"), "nothing was taken for it")
}

func TestRestore_AnUnclaimedSupervisorThatCannotBeEndedFaultsOrFails(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.inState(t, "created", machine.StateCreated)
	require.NoError(t, e.sup.Start(ctx, "created", someProcess, someUser))
	e.sup.FailAt("terminate", errBoom)

	svc := e.reopened(t)
	m, err := svc.Inspect(ctx, "created")
	require.NoError(t, err)
	assert.Equal(t, machine.StateFaulted, m.State)
	assert.Contains(t, m.FailureReason, errBoom.Error())
	rec, _ := e.store.Record("created")
	assert.Equal(t, machine.StateFaulted, rec.Machine.State, "the finding is recorded")

	// An unrecorded supervisor is not ended, so its failure to end is not met: the daemon serves.
	require.NoError(t, e.sup.Start(ctx, "ghost", someProcess, someUser))
	require.NoError(t, machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels())).Restore(ctx))
}

// Delete of a Machine restored from an unreadable record removes the record
// and passes no empty identifier to the Disk port; the control is a readable
// Machine, whose root disk is removed.
func TestDelete_UnreadableRecordSkipsTheDiskPort(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.created(t, "readable")
	require.NoError(t, e.store.Save(ctx, machine.Record{ID: "unreadable", Unreadable: "decode: bad"}))
	svc := e.reopened(t)

	u, err := svc.Inspect(ctx, "unreadable")
	require.NoError(t, err)
	require.Equal(t, machine.StateFaulted, u.State)
	require.Empty(t, u.RootDisk, "the setup is the case under test: no root disk name")

	before := e.disks.Calls("remove")
	require.NoError(t, svc.Delete(ctx, "unreadable"))
	assert.Equal(t, before, e.disks.Calls("remove"), "the Disk port is not called")
	_, kept := e.store.Record("unreadable")
	assert.False(t, kept, "the record is removed")

	require.NoError(t, svc.Delete(ctx, "readable"))
	assert.Equal(t, before+1, e.disks.Calls("remove"), "control: a readable Machine's root disk is removed")
}

// An orphan is not a Machine: it takes no call, it is listed until its supervisor has ended, and
// the daemon's look at the supervisors drops it then. Its state directory is not swept while it
// runs, and is once it has ended and no record holds it (ADR 0071).
func TestRestore_AnOrphanIsListedUntilItsSupervisorEnds(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	require.NoError(t, e.sup.Start(ctx, "ghost", someProcess, someUser))

	svc := e.reopened(t)
	require.Equal(t, []machine.MachineID{"ghost"}, svc.Orphans(ctx))
	requireKind(t, svc.Start(ctx, "ghost"), machine.ErrNotFound)

	machine.ObserveEnds(ctx, svc)
	assert.Equal(t, []machine.MachineID{"ghost"}, svc.Orphans(ctx), "control: while its supervisor lives it stays")

	require.NoError(t, e.sup.Terminate(ctx, "ghost"))
	machine.ObserveEnds(ctx, svc)
	assert.Empty(t, svc.Orphans(ctx), "once its supervisor has ended it is no longer listed")

	again := e.reopened(t)
	assert.Empty(t, again.Orphans(ctx), "a start that finds none lists none")
	assert.False(t, e.sup.HasState("ghost"), "and the state directory it left is swept, as for any Machine with no record")
}
