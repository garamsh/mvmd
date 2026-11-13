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

// restartEnv is an env whose lifecycle knows the host's boot, which the test
// changes with boot.Reboot.
func restartEnv() (*env, *memory.BootIdentity) {
	boot := memory.NewBootIdentity("boot-1")
	return newEnvWith(hotplugAndResize, machine.WithBootIdentity(boot)), boot
}

// createdRestarting creates the Machine, with RestartOnBoot as given.
func (e *env) createdRestarting(t *testing.T, id machine.MachineID, restart bool) {
	t.Helper()
	spec := baseSpec()
	spec.RestartOnBoot = restart
	require.NoError(t, e.svc.Create(context.Background(), id, request(id), spec))
}

// restarted is the lifecycle a restarted daemon builds: the VMMs that ran are
// gone and its Restore has run.
func (e *env) restarted(t *testing.T, boot *memory.BootIdentity) machine.Service {
	t.Helper()
	ctx := context.Background()
	ids, err := e.sup.Live(ctx)
	require.NoError(t, err)
	for _, id := range ids {
		require.NoError(t, e.sup.Terminate(ctx, id))
	}
	return e.reopenedWith(t, machine.WithBootIdentity(boot))
}

func inspected(t *testing.T, svc machine.Service, id machine.MachineID) machine.Machine {
	t.Helper()
	m, err := svc.Inspect(context.Background(), id)
	require.NoError(t, err)
	return m
}

func TestRestartOnBoot_ARunningMachineIsStartedAtTheFirstPermitOfTheNewBoot(t *testing.T) {
	ctx := context.Background()
	e, boot := restartEnv()
	for id, restart := range map[machine.MachineID]bool{"restart": true, "plain": false, "was-stopped": true} {
		e.createdRestarting(t, id, restart)
		require.NoError(t, e.svc.Start(ctx, id))
	}
	require.NoError(t, e.svc.Stop(ctx, "was-stopped", time.Second))
	assert.True(t, e.requireState(t, "restart", machine.StateRunning).RestartOnBoot, "Inspect reports the field")
	assert.False(t, e.requireState(t, "plain", machine.StateRunning).RestartOnBoot)

	boot.Reboot("boot-2")
	svc := e.restarted(t, boot)
	for _, id := range []machine.MachineID{"restart", "plain", "was-stopped"} {
		assert.Equal(t, machine.StateStopped, inspected(t, svc, id).State, "Restore settles %q as before", id)
	}
	assert.Contains(t, inspected(t, svc, "restart").FailureReason, "the VMM ended while mvmd was not running: ")

	e.permit.FailAt("permit", errBoom)
	machine.RestartDue(ctx, svc)
	assert.Equal(t, machine.StateStopped, inspected(t, svc, "restart").State, "nothing starts before the deadline is armed")

	e.permit.Heal("permit")
	machine.RestartDue(ctx, svc)
	assert.Equal(t, machine.StateRunning, inspected(t, svc, "restart").State, "the Machine is started again")
	assert.Equal(t, machine.StateStopped, inspected(t, svc, "plain").State, "a Machine without the field stays Stopped")
	assert.Equal(t, machine.StateStopped, inspected(t, svc, "was-stopped").State, "a Machine that was Stopped stays Stopped")
	states := e.events.States("restart")
	assert.Equal(t, []machine.State{machine.StateStopped, machine.StateRunning}, states[len(states)-2:], "the usual state events")
}

func TestRestartOnBoot_AVMMThatEndedInTheSameBootIsNotStarted(t *testing.T) {
	ctx := context.Background()
	e, boot := restartEnv()
	e.createdRestarting(t, "old-boot", true)
	require.NoError(t, e.svc.Start(ctx, "old-boot"))
	boot.Reboot("boot-2")
	e.createdRestarting(t, "same-boot", true)
	require.NoError(t, e.svc.Start(ctx, "same-boot"))

	svc := e.restarted(t, boot)
	machine.RestartDue(ctx, svc)
	assert.Equal(t, machine.StateRunning, inspected(t, svc, "old-boot").State, "control: the VMM that ended with the old boot is started")
	assert.Equal(t, machine.StateStopped, inspected(t, svc, "same-boot").State, "the VMM that ended in its boot is not")
}

func TestRestartOnBoot_AStopPauseOrDeleteAfterTheMachineRanCancelsTheRestart(t *testing.T) {
	ctx := context.Background()
	e, boot := restartEnv()
	for _, id := range []machine.MachineID{"stopped", "paused", "deleted", "control"} {
		e.createdRestarting(t, id, true)
		require.NoError(t, e.svc.Start(ctx, id))
	}
	require.NoError(t, e.svc.Pause(ctx, "paused"))
	boot.Reboot("boot-2")
	svc := e.restarted(t, boot)
	require.NoError(t, svc.Stop(ctx, "stopped", time.Second))
	require.NoError(t, svc.Delete(ctx, "deleted"))

	machine.RestartDue(ctx, svc)
	assert.Equal(t, machine.StateRunning, inspected(t, svc, "control").State, "control: left alone it is started")
	assert.Equal(t, machine.StateStopped, inspected(t, svc, "stopped").State, "a Stop after it ran cancels the restart")
	assert.Equal(t, machine.StateStopped, inspected(t, svc, "paused").State, "a Machine that was Paused is not started")
	_, err := svc.Inspect(ctx, "deleted")
	assert.ErrorIs(t, err, machine.ErrNotFound)
}

func TestRestartOnBoot_AFailedRestartLeavesTheMachineStoppedWithTheReasonAndIsNotRetried(t *testing.T) {
	ctx := context.Background()
	e, boot := restartEnv()
	e.createdRestarting(t, "m", true)
	require.NoError(t, e.svc.Start(ctx, "m"))
	boot.Reboot("boot-2")
	svc := e.restarted(t, boot)

	e.driver.FailAt("boot", errBoom)
	machine.RestartDue(ctx, svc)
	m := inspected(t, svc, "m")
	assert.Equal(t, machine.StateStopped, m.State)
	assert.Contains(t, m.FailureReason, "the restart after the host's boot changed failed")
	assert.Contains(t, m.FailureReason, errBoom.Error())
	rec, _ := e.store.Record("m")
	assert.Equal(t, m.FailureReason, rec.Machine.FailureReason, "the reason is recorded")

	e.driver.Heal("boot")
	calls := e.driver.Calls("boot")
	machine.RestartDue(ctx, svc)
	assert.Equal(t, calls, e.driver.Calls("boot"), "it is not retried")
	assert.Equal(t, machine.StateStopped, inspected(t, svc, "m").State)
}

func TestRestartOnBoot_AStartTheHostEndedBeforeItFinishedIsRestartedOnlyOnANewBoot(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		reboot bool
		want   machine.State
	}{{"new boot", true, machine.StateRunning}, {"same boot", false, machine.StateCreated}} {
		t.Run(tc.name, func(t *testing.T) {
			e, boot := restartEnv()
			e.createdRestarting(t, "m", true)
			// The record a start leaves when the host goes down inside it.
			rec, _ := e.store.Record("m")
			rec.Machine.StartedInBoot = "boot-1"
			require.NoError(t, e.store.Save(ctx, rec))
			if tc.reboot {
				boot.Reboot("boot-2")
			}
			svc := e.restarted(t, boot)
			machine.RestartDue(ctx, svc)
			assert.Equal(t, tc.want, inspected(t, svc, "m").State)
		})
	}
}
