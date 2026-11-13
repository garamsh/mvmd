package app_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/app"
	fencingmemory "github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// startOnClock is start with the clock the fencing deadline and the Machines'
// boot identity read, so a test can end the host's boot between two daemons.
// kept says the node holds the root disks of an earlier daemon's Machines.
func startOnClock(t *testing.T, cfg app.Config, clock *fencingmemory.Clock, kept bool) *node {
	t.Helper()
	a, err := app.NewWithClock(cfg, clock)
	require.NoError(t, err)
	provide(a)
	// The disks a node keeps across the daemon's restart, which the memory
	// substitutes do not.
	if kept {
		for _, id := range []string{"restart", "plain", "was-stopped"} {
			a.Substitutes.Disks.Provide(machine.DiskID(id + "-root"))
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	n := &node{app: a, cfg: cfg, stop: cancel, done: make(chan error, 1)}
	go func() { n.done <- a.Serve(ctx) }()
	select {
	case <-a.Ready():
	case err := <-n.done:
		t.Fatalf("the daemon ended before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon was not ready")
	}
	t.Cleanup(func() { n.shutdown(t) })
	dial, cancelDial := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelDial()
	n.conn, err = runtimeapi.Dial(dial, cfg.Socket.Path)
	require.NoError(t, err)
	n.client = v1alpha2.NewRuntimeServiceClient(n.conn)
	return n
}

func stateOfMachine(t *testing.T, n *node, id string) v1alpha2.MachineState {
	t.Helper()
	got, err := n.client.InspectMachine(context.Background(), &v1alpha2.InspectMachineRequest{MachineId: id})
	require.NoError(t, err)
	return got.GetMachine().GetState()
}

// A Machine created with restart_on_boot that was Running when the host went
// down is started at the first arming of the deadline in the new boot; one
// without the field, and one that was Stopped, stay Stopped; and a VMM that
// ended in the boot it started in is not started again (ADR 0062). The
// restarted daemon is the wired one, and the host's reboot is a clock that
// reports another boot.
func TestRestartOnBoot_TheWiredDaemonStartsARunningMachineAtTheFirstArmingOfTheNewBoot(t *testing.T) {
	ctx := context.Background()
	cfg := testConfig(t)
	clock := fencingmemory.NewClock("boot-1")

	first := startOnClock(t, cfg, clock, false)
	for id, restart := range map[string]bool{"restart": true, "plain": false, "was-stopped": true} {
		req := create(id)
		req.RestartOnBoot = restart
		_, err := first.client.CreateMachine(ctx, req)
		require.NoError(t, err)
	}
	renew(t, first, 1)
	for _, id := range []string{"restart", "plain", "was-stopped"} {
		_, err := first.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: id})
		require.NoError(t, err)
	}
	_, err := first.client.StopMachine(ctx, &v1alpha2.StopMachineRequest{MachineId: "was-stopped"})
	require.NoError(t, err)
	first.shutdown(t)

	clock.Reboot("boot-2")
	second := startOnClock(t, cfg, clock, true)
	for _, id := range []string{"restart", "plain", "was-stopped"} {
		assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, stateOfMachine(t, second, id), "%s before the deadline is armed", id)
	}
	renew(t, second, 1)
	require.Eventually(t, func() bool {
		return stateOfMachine(t, second, "restart") == v1alpha2.MachineState_MACHINE_STATE_RUNNING
	}, 10*time.Second, 50*time.Millisecond, "started at the first arming in the new boot")
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, stateOfMachine(t, second, "plain"), "no field")
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, stateOfMachine(t, second, "was-stopped"), "it was Stopped before")
	second.shutdown(t)

	// The same boot: the VMM that ran in boot-2 and ended with the daemon's
	// absence is not started, though the deadline is armed.
	third := startOnClock(t, cfg, clock, true)
	assert.Never(t, func() bool {
		return stateOfMachine(t, third, "restart") == v1alpha2.MachineState_MACHINE_STATE_RUNNING
	}, 2500*time.Millisecond, 100*time.Millisecond, "a VMM that ended in its boot is not started again")
}
