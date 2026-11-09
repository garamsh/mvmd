package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// agentStopsTheGuest makes the guest's agent end the Machine's VMM when it is
// asked to terminate, as an entrypoint that stops on SIGTERM does.
func (e *env) agentStopsTheGuest() {
	e.guest.EndsMachineWith(func(id machine.MachineID) {
		_ = e.sup.Terminate(context.Background(), id)
	})
}

// agentTakesTheRequestAndIgnoresIt makes the agent answer the request while the
// guest goes on running.
func (e *env) agentTakesTheRequestAndIgnoresIt() {
	e.guest.EndsMachineWith(func(machine.MachineID) {})
}

// stopTime is how long a Stop of the Machine took.
func (e *env) stopTime(t *testing.T, id machine.MachineID, grace time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	require.NoError(t, e.svc.Stop(context.Background(), id, grace))
	return time.Since(start)
}

// A graceful stop asks the agent first and ends when the VMM does, before the
// grace passes, and the control is a guest that goes on running, whose stop
// waits out the grace.
func TestStop_AnAgentThatStopsTheGuestEndsTheStopBeforeTheGrace(t *testing.T) {
	const grace = 400 * time.Millisecond

	t.Run("the guest stops on the request", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.agentStopsTheGuest()

		took := e.stopTime(t, "m", time.Hour)
		assert.Less(t, took, 5*time.Second, "the stop waited for the grace after the VMM had ended")
		e.requireState(t, "m", machine.StateStopped)
		_, running := e.sup.Running("m")
		assert.False(t, running)
		assert.Equal(t, 1, e.guest.Terminated("m"))
		assert.Zero(t, e.driver.Calls("shutdown"), "the Driver's request is only the fallback")
	})

	t.Run("control: a guest that ignores the request holds the stop to the grace", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.agentTakesTheRequestAndIgnoresIt()

		took := e.stopTime(t, "m", grace)
		assert.GreaterOrEqual(t, took, grace)
		assert.Equal(t, 1, e.guest.Terminated("m"))
	})
}

// A guest that ignores the request is forced down at the grace: the stop ends
// with the VMM terminated, not before the grace, and by the supervisor.
func TestStop_AGuestThatIgnoresTheRequestIsForcedDownAtTheGrace(t *testing.T) {
	const grace = 300 * time.Millisecond

	t.Run("the VMM is ended at the grace", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.agentTakesTheRequestAndIgnoresIt()

		took := e.stopTime(t, "m", grace)
		assert.GreaterOrEqual(t, took, grace, "forced before the grace was spent")
		assert.Less(t, took, 5*time.Second)
		e.requireState(t, "m", machine.StateStopped)
		_, running := e.sup.Running("m")
		assert.False(t, running, "a stop always ends with the VMM terminated")
		assert.Equal(t, 1, e.sup.Calls("terminate"), "the supervisor ended it")
		assert.Zero(t, e.driver.Calls("shutdown"))
	})

	t.Run("control: a guest that obeys is not forced", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.agentStopsTheGuest()

		e.stopTime(t, "m", grace)
		assert.Equal(t, 2, e.sup.Calls("terminate"), "the guest's own end, then the lifecycle's confirmation, which finds nothing to end")
	})

	t.Run("an agent that refuses is forced at once", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.RefuseTerminate(errBoom)

		took := e.stopTime(t, "m", time.Hour)
		assert.Less(t, took, 5*time.Second, "nothing was waited for")
		e.requireState(t, "m", machine.StateStopped)
		_, running := e.sup.Running("m")
		assert.False(t, running)
		assert.Zero(t, e.driver.Calls("shutdown"), "the agent answered, so it is not unreachable")
	})
}

// An agent that cannot be reached leaves the Driver's request, with the time
// the grace has left, and the control is an agent that answers, which the
// Driver is never asked after.
func TestStop_AnUnreachableAgentFallsBackToTheDriver(t *testing.T) {
	const grace = 7 * time.Second

	t.Run("the Driver is asked", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("terminate", errBoom)

		require.NoError(t, e.svc.Stop(context.Background(), "m", grace))
		assert.Equal(t, 1, e.driver.Calls("shutdown"))
		assert.Positive(t, e.driver.Grace())
		assert.LessOrEqual(t, e.driver.Grace(), grace, "the call to the agent spent some of the grace")
		e.requireState(t, "m", machine.StateStopped)
		_, running := e.sup.Running("m")
		assert.False(t, running)
	})

	t.Run("control: an agent that answers leaves the Driver alone", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.agentStopsTheGuest()

		require.NoError(t, e.svc.Stop(context.Background(), "m", grace))
		assert.Zero(t, e.driver.Calls("shutdown"))
	})

	t.Run("a fallback that the guest ignores is forced down", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.guest.FailAt("terminate", errBoom)
		e.driver.FailAt("shutdown", errBoom)

		require.NoError(t, e.svc.Stop(context.Background(), "m", time.Millisecond))
		e.requireState(t, "m", machine.StateStopped)
		_, running := e.sup.Running("m")
		assert.False(t, running)
	})
}

// A forced stop sends nothing to the guest, and a stop of a Machine that is
// not running does not ask it either.
func TestStop_AsksNothingWhereItShouldNot(t *testing.T) {
	ctx := context.Background()

	t.Run("ForceStop", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.agentStopsTheGuest()
		require.NoError(t, e.svc.ForceStop(ctx, "m"))
		assert.Zero(t, e.guest.Terminated("m"))
	})

	t.Run("a Stopped Machine", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateStopped)
		e.agentStopsTheGuest()
		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
		assert.Zero(t, e.guest.Terminated("m"))
	})

	t.Run("control: a Running Machine is asked", func(t *testing.T) {
		e := newEnv()
		e.inState(t, "m", machine.StateRunning)
		e.agentStopsTheGuest()
		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
		assert.Equal(t, 1, e.guest.Terminated("m"))
	})
}
