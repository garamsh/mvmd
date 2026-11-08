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
	svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.store, e.permit)
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

func TestRestore_NoRunningMachineNeedsNoListing(t *testing.T) {
	e := newEnv(hotplugAndResize...)
	e.inState(t, "c", machine.StateCreated)
	e.inState(t, "s", machine.StateStopped)
	e.sup.FailAt("live", errBoom)

	svc := e.reopened(t)
	m, err := svc.Inspect(context.Background(), "c")
	require.NoError(t, err)
	assert.Equal(t, machine.StateCreated, m.State)
}
