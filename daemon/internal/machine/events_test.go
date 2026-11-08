package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	created = machine.StateCreated
	running = machine.StateRunning
	paused  = machine.StatePaused
	stopped = machine.StateStopped
	faulted = machine.StateFaulted
)

func (e *env) requireStates(t *testing.T, id machine.MachineID, want ...machine.State) {
	t.Helper()
	assert.Equal(t, want, e.events.States(id), "states reported for %q", id)
}

// Every committed state change is reported, whichever path committed it.
func TestEvents_EveryStateTheLifecycleCommitsIsReported(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		do   func(t *testing.T, e *env)
		want []machine.State
	}{
		{"create", func(t *testing.T, e *env) { e.created(t, "m") }, []machine.State{created}},
		{"start", func(t *testing.T, e *env) { e.inState(t, "m", running) }, []machine.State{created, running}},
		{"pause", func(t *testing.T, e *env) { e.inState(t, "m", paused) }, []machine.State{created, running, paused}},
		{"resume", func(t *testing.T, e *env) {
			e.inState(t, "m", paused)
			require.NoError(t, e.svc.Resume(ctx, "m"))
		}, []machine.State{created, running, paused, running}},
		{"stop", func(t *testing.T, e *env) { e.inState(t, "m", stopped) }, []machine.State{created, running, stopped}},
		{"force stop", func(t *testing.T, e *env) {
			e.inState(t, "m", running)
			require.NoError(t, e.svc.ForceStop(ctx, "m"))
		}, []machine.State{created, running, stopped}},
		{"a stop that cannot be confirmed", func(t *testing.T, e *env) { e.inState(t, "m", faulted) },
			[]machine.State{created, running, faulted}},
		{"a stop whose network cannot be removed", func(t *testing.T, e *env) {
			e.inState(t, "m", running)
			e.net.FailAt("withdraw", errBoom)
			require.ErrorIs(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		}, []machine.State{created, running, faulted}},
		{"a boot that failed", func(t *testing.T, e *env) {
			e.created(t, "m")
			e.driver.FailAt("boot", errBoom)
			require.ErrorIs(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		}, []machine.State{created, stopped}},
		{"a boot that failed and whose VMM cannot be ended", func(t *testing.T, e *env) {
			e.created(t, "m")
			e.driver.FailAt("boot", errBoom)
			e.sup.FailAt("terminate", errBoom)
			require.ErrorIs(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		}, []machine.State{created, faulted}},
		{"a start that cannot be recorded", func(t *testing.T, e *env) {
			e.created(t, "m")
			e.store.FailAt("save", errBoom)
			require.ErrorIs(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		}, []machine.State{created, faulted}},
		{"a pause that cannot be recorded", func(t *testing.T, e *env) {
			e.inState(t, "m", running)
			e.store.FailAt("save", errBoom)
			require.ErrorIs(t, e.svc.Pause(ctx, "m"), machine.ErrFailed)
		}, []machine.State{created, running, faulted}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEnv(hotplugAndResize...)
			tc.do(t, e)
			e.requireStates(t, "m", tc.want...)
		})
	}
}

// Controls for the above: a call that changes no state reports none.
func TestEvents_ACallThatChangesNoStateReportsNothing(t *testing.T) {
	ctx := context.Background()

	t.Run("a refused call", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", created)
		require.ErrorIs(t, e.svc.Pause(ctx, "m"), machine.ErrPreconditionFailed)
		e.requireStates(t, "m", created)
	})
	t.Run("a start that failed before the VMM and left the Machine as it was", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", created)
		e.driver.FailAt("describe", errBoom)
		require.ErrorIs(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
		e.requireStates(t, "m", created)
	})
	t.Run("a repeat in the target state", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", running)
		require.NoError(t, e.svc.Start(ctx, "m"))
		e.requireStates(t, "m", created, running)
	})
	t.Run("a commit that keeps the state", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", running)
		require.NoError(t, e.svc.SetResources(ctx, "m", machine.Resources{VCPUs: 4, MemoryBytes: 1 << 30}))
		m := e.requireState(t, "m", running)
		require.Equal(t, uint32(4), m.Resources.VCPUs, "the commit happened")
		e.requireStates(t, "m", created, running)
	})
	t.Run("a Create that failed", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.disks.FailAt("create-root", errBoom)
		require.Error(t, e.svc.Create(ctx, "m", request("m"), baseSpec()))
		assert.Empty(t, e.events.Changes())
	})
	t.Run("a Delete", func(t *testing.T) {
		// A deleted Machine has no state to report: its deletion is its own
		// event.
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", stopped)
		require.NoError(t, e.svc.Delete(ctx, "m"))
		e.requireStates(t, "m", created, running, stopped)
	})
}

func TestEvents_ADeletionIsReportedOnceItCommits(t *testing.T) {
	ctx := context.Background()

	t.Run("a Delete reports the Machine, once", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", stopped)
		e.created(t, "control")
		require.NoError(t, e.svc.Delete(ctx, "m"))
		assert.Equal(t, []machine.MachineID{"m"}, e.events.Deletions())

		// A repeat finds it absent and reports nothing more.
		require.NoError(t, e.svc.Delete(ctx, "m"))
		assert.Equal(t, []machine.MachineID{"m"}, e.events.Deletions())
	})
	t.Run("a Faulted Machine's Delete is reported", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", faulted)
		require.NoError(t, e.svc.Delete(ctx, "m"))
		assert.Equal(t, []machine.MachineID{"m"}, e.events.Deletions())
	})
	t.Run("a refused Delete reports nothing", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", running)
		require.ErrorIs(t, e.svc.Delete(ctx, "m"), machine.ErrPreconditionFailed)
		assert.Empty(t, e.events.Deletions())
	})
	t.Run("a Delete that failed before the record was removed reports nothing", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", stopped)
		e.store.FailAt("remove", errBoom)
		require.ErrorIs(t, e.svc.Delete(ctx, "m"), machine.ErrFailed)
		assert.Empty(t, e.events.Deletions())

		// Control: the same Delete reports once the step stops failing.
		e.store.Heal("remove")
		require.NoError(t, e.svc.Delete(ctx, "m"))
		assert.Equal(t, []machine.MachineID{"m"}, e.events.Deletions())
	})
}

func TestEvents_ARestartThatSettlesAMachineReportsIt(t *testing.T) {
	ctx := context.Background()

	t.Run("a Machine whose supervisor is gone is Stopped", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", running)
		require.NoError(t, e.sup.Terminate(ctx, "m"))
		e.reopened(t)
		e.requireStates(t, "m", created, running, stopped)
	})
	t.Run("control: a live supervisor settles nothing", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", running)
		e.reopened(t)
		e.requireStates(t, "m", created, running)
	})
	t.Run("supervisors that cannot be listed leave a running Machine Faulted", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", running)
		e.sup.FailAt("live", errBoom)
		e.reopened(t)
		e.requireStates(t, "m", created, running, faulted)
	})
}

func TestEvents_TheDriversEventsAreReported(t *testing.T) {
	e := newEnv(hotplugAndResize...)
	assert.Empty(t, e.events.DriverEvents(), "nothing before the Driver raises one")

	e.driver.Raise("vmm-exited", "signal 9")
	assert.Equal(t, []machine.DriverEvent{{Name: "vmm-exited", Detail: "signal 9"}}, e.events.DriverEvents())
	assert.Empty(t, e.events.Changes(), "a Driver event is not a state change")
}
