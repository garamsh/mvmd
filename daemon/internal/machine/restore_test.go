package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// reopened is a second lifecycle over the same store, as a restarted daemon
// builds it, with the substitutes the test chooses to keep or empty.
func (e *env) reopened(t *testing.T) machine.Service {
	t.Helper()
	svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.store, e.permit, machine.UIDRange{First: 1000, Count: 256}, e.events)
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
			assert.Equal(t, "the VMM ended while mvmd was not running", m.FailureReason)
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

var someProcess = machine.ProcessDescription{Executable: "vmm"}
var someUser = machine.User{UID: 1000, GID: 1000}

// A supervisor no Running or Paused record claims is ended; the control is the
// supervisor of a Running record, which is kept.
func TestRestore_EndsSupervisorsNoRunningRecordClaims(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.inState(t, "running", machine.StateRunning)
	e.inState(t, "created", machine.StateCreated)
	e.inState(t, "stopped", machine.StateStopped)
	for _, id := range []machine.MachineID{"created", "stopped", "ghost"} {
		require.NoError(t, e.sup.Start(ctx, id, someProcess, someUser))
	}
	_, err := e.net.Apply(ctx, "created", machine.NetworkPolicy{})
	require.NoError(t, err)

	svc := e.reopened(t)

	_, alive := e.sup.Running("running")
	assert.True(t, alive, "control: the supervisor of a Running record is kept")
	for _, id := range []machine.MachineID{"created", "stopped", "ghost"} {
		_, alive := e.sup.Running(id)
		assert.False(t, alive, "the supervisor of %q is ended", id)
	}
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

	// An unrecorded supervisor is an error, so the daemon does not serve.
	require.NoError(t, e.sup.Start(ctx, "ghost", someProcess, someUser))
	err = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.store, e.permit, machine.UIDRange{First: 1000, Count: 256}, e.events).Restore(ctx)
	require.ErrorIs(t, err, errBoom)
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
