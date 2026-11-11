package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

const endedOnItsOwn = "the VMM ended with no call asking it to: "

// observed ends the Machine's supervisor, as its VMM's end does, and looks once.
func (e *env) observed(t *testing.T, id machine.MachineID) {
	t.Helper()
	require.NoError(t, e.sup.Terminate(context.Background(), id))
	machine.ObserveEnds(context.Background(), e.svc)
}

func TestVMMEnd_RunningAndPausedMachinesBecomeStopped(t *testing.T) {
	for _, state := range []machine.State{machine.StateRunning, machine.StatePaused} {
		t.Run(state.String(), func(t *testing.T) {
			e := newEnv(hotplugAndResize...)
			e.inState(t, "m", state)
			e.driver.WriteConsoleLog("m", agent+"entrypoint exited code=3\n")
			before := len(e.events.States("m"))

			e.observed(t, "m")

			m := e.requireState(t, "m", machine.StateStopped)
			assert.Equal(t, endedOnItsOwn+"the entrypoint exited with code 3", m.FailureReason)
			e.requireStored(t, "m", machine.StateStopped)
			_, tap := e.net.Applied("m")
			assert.False(t, tap, "the network is withdrawn as Stop withdraws it")
			states := e.events.States("m")
			require.Len(t, states, before+1)
			assert.Equal(t, machine.StateStopped, states[before])

			record, err := e.svc.ReadExecOutput(context.Background(), "m", "entrypoint", 0, nil)
			require.NoError(t, err)
			assert.Equal(t, machine.ExecRecord{ID: "entrypoint", Status: machine.ExecExited, ExitCode: 3}, record)

			machine.ObserveEnds(context.Background(), e.svc)
			assert.Len(t, e.events.States("m"), before+1, "a Stopped Machine is not transitioned again")
		})
	}
}

func TestVMMEnd_ReasonNamesTheCause(t *testing.T) {
	tests := []struct {
		name, log, want string
	}{
		{"exit code", agent + "entrypoint exited code=0\n", "the entrypoint exited with code 0"},
		{"signal", agent + "entrypoint exited signal=9\n", "the entrypoint was ended by signal 9"},
		{"agent failure", agent + "failed to power off: EPERM\n", "the Guest Agent ended: failed to power off: EPERM"},
		{"guest panic", agent + "panic: index out of bounds\n", "the Guest Agent ended: panic: index out of bounds"},
		{"not started", agent + "entrypoint not started reason=no such file\n", "the entrypoint did not start: no such file"},
		{"no line", "[    1] booted\n", "its console log has no line of the Guest Agent about the entrypoint's end, as when the VMM was killed or crashed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv()
			e.inState(t, "m", machine.StateRunning)
			e.driver.WriteConsoleLog("m", tt.log)
			e.observed(t, "m")
			m := e.requireState(t, "m", machine.StateStopped)
			assert.Equal(t, endedOnItsOwn+tt.want, m.FailureReason)
		})
	}
}

func TestVMMEnd_OnlyTheMachineWhoseSupervisorIsGoneIsStopped(t *testing.T) {
	e := newEnv()
	e.inState(t, "ended", machine.StateRunning)
	e.inState(t, "alive", machine.StateRunning)
	e.observed(t, "ended")
	e.requireState(t, "ended", machine.StateStopped)
	e.requireState(t, "alive", machine.StateRunning)
}

func TestVMMEnd_ANetworkThatCannotBeWithdrawnFaultsTheMachine(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	e.net.FailAt("withdraw", errBoom)
	e.observed(t, "m")
	m := e.requireState(t, "m", machine.StateFaulted)
	assert.Contains(t, m.FailureReason, "the network could not be removed")
}

func TestVMMEnd_AListingThatFailsObservesNothing(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	require.NoError(t, e.sup.Terminate(context.Background(), "m"))
	e.sup.FailAt("live", errBoom)
	machine.ObserveEnds(context.Background(), e.svc)
	e.requireState(t, "m", machine.StateRunning)
	e.sup.Heal("live")
	machine.ObserveEnds(context.Background(), e.svc)
	e.requireState(t, "m", machine.StateStopped)
}

// A Stop whose Machine's VMM ends under it is the Stop's: the look does not
// move the Machine while the call holds it, and does not move it after.
func TestVMMEnd_DoesNotRaceAStopTheVMMsEndOvertakes(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.inState(t, "m", machine.StateRunning)
	before := len(e.events.States("m"))
	entered, release := e.driver.HoldShutdown()

	stopped := make(chan error, 1)
	go func() { stopped <- e.svc.Stop(ctx, "m", time.Hour) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never reached the guest")
	}
	require.NoError(t, e.sup.Terminate(ctx, "m"), "the VMM ends while the stop waits")

	machine.ObserveEnds(ctx, e.svc)
	assert.Len(t, e.events.States("m"), before, "the look left the Machine to the stop")

	release()
	require.NoError(t, <-stopped)
	m := e.requireState(t, "m", machine.StateStopped)
	assert.Empty(t, m.FailureReason, "the Machine was stopped by a call, not by its VMM's own end")
	machine.ObserveEnds(ctx, e.svc)
	assert.Len(t, e.events.States("m"), before+1, "one transition, not two")
}

// The listing that names a Machine is older than the lock: a Machine whose
// supervisor exists when it is held is not found ended.
func TestVMMEnd_ALookIsNotFooledByAStaleListing(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.inState(t, "m", machine.StateRunning)
	before := len(e.events.States("m"))
	machine.SettleEnd(ctx, e.svc, "m")
	e.requireState(t, "m", machine.StateRunning)
	assert.Len(t, e.events.States("m"), before)
}

// The expiry kills the supervisors, releases its ordering and only then
// force-stops the Machines; a look in that gap leaves the Machine to the
// ForceStop, which is not reported as the VMM's own end.
func TestVMMEnd_ALookBetweenTheExpiryKillingAndItsForceStopLeavesTheMachine(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.inState(t, "m", machine.StateRunning)
	before := len(e.events.States("m"))
	e.permit.FailAt("permit", errBoom) // the deadline has expired
	e.observed(t, "m")                 // the supervisor is gone
	e.requireState(t, "m", machine.StateRunning)
	assert.Len(t, e.events.States("m"), before)

	require.NoError(t, e.svc.ForceStop(ctx, "m"))
	m := e.requireState(t, "m", machine.StateStopped)
	assert.Empty(t, m.FailureReason, "the expiry's stop is not the VMM's own end")
	assert.Len(t, e.events.States("m"), before+1)
}

func TestVMMEnd_WatchObservesWhileItRuns(t *testing.T) {
	e := newEnv()
	e.inState(t, "m", machine.StateRunning)
	svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels()), machine.WithVMMEndPoll(5*time.Millisecond))
	require.NoError(t, svc.Restore(context.Background()))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { svc.Watch(ctx); close(done) }()
	time.Sleep(100 * time.Millisecond) // many looks find the Machine alive
	m, err := svc.Inspect(ctx, "m")
	require.NoError(t, err)
	require.Equal(t, machine.StateRunning, m.State)
	require.NoError(t, e.sup.Terminate(ctx, "m"))
	require.Eventually(t, func() bool {
		m, err := svc.Inspect(ctx, "m")
		return err == nil && m.State == machine.StateStopped
	}, 5*time.Second, 5*time.Millisecond)
	cancel()
	<-done
}
