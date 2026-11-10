//go:build e2e_real

package e2e_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// shortDeadline is the length the tests renew to when a deadline is to pass with a Machine
// running; pastExpiry is how long after it a test that expects the Machine to live on keeps
// looking, which is far more than the time a supervisor takes to act on an expiry.
const (
	shortDeadline = 20 * time.Second
	pastExpiry    = 15 * time.Second
)

// A renewal that extends the deadline reaches the supervisor of a running Machine: the Machine
// is still running after the expiry the supervisor held before the renewal, and Inspect reports the
// renewed deadline. A supervisor that kept the earlier expiry ends the VMM at it.
func TestE2E_ARenewalExtendsTheDeadlineOfARunningMachine(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "extended")
	before := n.vmmPIDs(t, "extended")
	require.NotEmpty(t, before, "the Machine's VMM runs in its cgroup")

	n.renew(t, shortDeadline, 2)
	// The expiry is no later than the moment the renewal is acknowledged plus its length.
	expiry := time.Now().Add(shortDeadline)
	n.renew(t, time.Hour, 3)

	// The wait is for the clock to pass the expiry the supervisor held, which is the condition
	// the assertion below depends on: the VMM that is there after it was not ended at it.
	time.Sleep(time.Until(expiry.Add(pastExpiry)))

	assert.Equal(t, before, n.vmmPIDs(t, "extended"), "the VMM that ran before the renewal runs after the earlier expiry")
	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "extended"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, inspected.Machine.State)
	out, code := n.exec(t, ctx, "extended", "after-expiry", "/bin/entry", "echo", "still here")
	assert.Equal(t, "still here\n", out)
	assert.Equal(t, int32(0), code)

	status := n.deadline(t)
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, status.Phase)
	assert.Equal(t, time.Hour, status.Length.AsDuration())
	assert.Equal(t, uint64(3), status.LastRenewalNumber)
	assert.Greater(t, status.Remaining.AsDuration(), 30*time.Minute, "the time left is of the renewed deadline, not the earlier one")
}

// A deadline that passes with no further renewal ends the running Machine's VMM and stops the
// Machine, with its disk retained, and Inspect reports that everything stopped.
func TestE2E_ThePassedDeadlineStopsARunningMachine(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "fenced")
	require.NotEmpty(t, n.vmmPIDs(t, "fenced"), "the Machine's VMM runs before the deadline passes")

	n.renew(t, shortDeadline, 2)

	n.waitVMMGone(t, "fenced")
	n.waitState(t, ctx, "fenced", v1alpha2.MachineState_MACHINE_STATE_STOPPED)
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_STOPPED_EVERYTHING, n.deadline(t).Phase)
	assert.Len(t, n.entries(t, "disks"), 1, "the expiry retains the disks")
}

// The supervisor holds the expiry a renewal moved it to and enforces it with the daemon down: the
// VMM ends at the deadline with no daemon running, and the daemon that starts again reports the
// Machine Stopped.
func TestE2E_TheSupervisorEndsTheVMMAtTheDeadlineWithTheDaemonDown(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "unattended")
	require.NotEmpty(t, n.vmmPIDs(t, "unattended"), "the Machine's VMM runs before the deadline passes")

	n.renew(t, shortDeadline, 2)
	// The expiry is no later than the moment the renewal is acknowledged plus its length.
	expiry := time.Now().Add(shortDeadline)
	n.stopDaemon()
	require.True(t, time.Now().Before(expiry), "the daemon was down before the deadline passed, so it did not end the VMM")
	require.NotEmpty(t, n.vmmPIDs(t, "unattended"), "the VMM runs with the daemon down, until the deadline")

	n.waitVMMGone(t, "unattended")

	n.startDaemon()
	n.waitState(t, ctx, "unattended", v1alpha2.MachineState_MACHINE_STATE_STOPPED)
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_STOPPED_EVERYTHING, n.deadline(t).Phase)
}

// A daemon stopped and started again while a Machine runs reattaches to it: the same VMM
// process, reported Running, with Exec into the guest succeeding, and the deadline it held.
func TestE2E_ARestartedDaemonReattachesToARunningMachine(t *testing.T) {
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n.startRunning(t, ctx, "survivor")
	before := n.vmmPIDs(t, "survivor")
	require.NotEmpty(t, before, "the Machine's VMM runs in its cgroup")

	n.restartDaemon()

	inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "survivor"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, inspected.Machine.State)
	assert.Equal(t, before, n.vmmPIDs(t, "survivor"), "the VMM the first daemon started is the one running")
	out, code := n.exec(t, ctx, "survivor", "after-restart", "/bin/entry", "echo", "reattached")
	assert.Equal(t, "reattached\n", out)
	assert.Equal(t, int32(0), code)

	status := n.deadline(t)
	assert.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, status.Phase)
	assert.Equal(t, uint64(1), status.LastRenewalNumber, "the renewal recorded before the restart")
}

// startRunning arms the deadline and brings a Machine of a bootable image to Running.
func (n *node) startRunning(t *testing.T, ctx context.Context, id string) {
	t.Helper()
	n.arm(t)
	pulled := n.pullBootable(t, id)
	_, err := n.client.CreateMachine(ctx, createRequest(id, pulled.Digest))
	require.NoError(t, err)
	started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: id})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State)
}

// deadline reads the node's fencing deadline through Inspect.
func (n *node) deadline(t *testing.T) *v1alpha2.DeadlineStatus {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	inspected, err := n.client.InspectDeadline(ctx, &v1alpha2.InspectDeadlineRequest{})
	require.NoError(t, err)
	return inspected.Status
}

// vmmPIDs lists the processes in the Machine's VMM cgroup, which the daemon makes for the VMM
// and removes once it has ended, so no cgroup is no process.
func (n *node) vmmPIDs(t *testing.T, machine string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(n.cgroups[1], machine, "cgroup.procs"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	require.NoError(t, err)
	return strings.Fields(string(raw))
}

// waitVMMGone waits until no process is left in the Machine's VMM cgroup.
func (n *node) waitVMMGone(t *testing.T, machine string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for len(n.vmmPIDs(t, machine)) > 0 {
		require.True(t, time.Now().Before(deadline), "the VMM of %s has not ended: processes %v", machine, n.vmmPIDs(t, machine))
		time.Sleep(250 * time.Millisecond)
	}
}

// waitState waits until the runtime API reports the Machine in the state.
func (n *node) waitState(t *testing.T, ctx context.Context, machine string, state v1alpha2.MachineState) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	for {
		inspected, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: machine})
		require.NoError(t, err)
		if inspected.Machine.State == state {
			return
		}
		require.True(t, time.Now().Before(deadline), "%s is %s, not %s, 2m0s on", machine, inspected.Machine.State, state)
		time.Sleep(250 * time.Millisecond)
	}
}
