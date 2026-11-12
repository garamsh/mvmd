//go:build e2e_real

package e2e_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// guestClock reads the guest's wall clock by an exec.
func (n *node) guestClock(t *testing.T, ctx context.Context, machine, id string) time.Time {
	t.Helper()
	out, code := n.exec(t, ctx, machine, id, "/bin/entry", "now")
	require.Equal(t, int32(0), code)
	nanos, err := strconv.ParseInt(strings.TrimSpace(out), 10, 64)
	require.NoError(t, err, "the guest printed %q", out)
	return time.Unix(0, nanos)
}

// A restore is followed by CatchUp (ADR 0016), which sets the guest's wall clock from the host's
// and applies the new Machine's network. The restore waits a gap longer than the clock's tolerance
// after the snapshot, so a restored guest that kept the snapshot's clock lags the host by the gap,
// and one that kept the snapshot's network dials from the origin's address. Each is shown not to
// be the origin's: the guest's clock is later than the origin's last reading by at least the gap,
// and the host's listener saw the restored guest's dial from the new address and none from the
// origin's.
func TestE2E_ARestoredGuestHasTheHostsClockAndTheNewMachinesNetwork(t *testing.T) {
	const gap = 5 * time.Second
	n := startNode(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	n.arm(t)
	pulled := n.pullBootable(t, "catchup")
	listener := listenOnHost(t)
	policy := func() *v1alpha2.NetworkPolicy {
		return &v1alpha2.NetworkPolicy{Egress: []*v1alpha2.NetworkRule{tcpAllow(gateway, listener.port)}}
	}
	target := fmt.Sprintf("%s:%d", gateway, listener.port)

	originAddress := n.policyMachine(t, ctx, "origin", pulled.Digest, policy())
	originClock := n.guestClock(t, ctx, "origin", "origin-now")

	_, err := n.client.PauseMachine(ctx, &v1alpha2.PauseMachineRequest{MachineId: "origin"})
	require.NoError(t, err)
	_, err = n.client.SnapshotMachine(ctx, &v1alpha2.SnapshotMachineRequest{SnapshotId: "snap", RequestId: "snapshot-snap", MachineId: "origin"})
	require.NoError(t, err)
	time.Sleep(gap)

	// The reading before the call is the lower bound: CatchUp sets the clock after the call began,
	// so a guest that caught up is not behind it, and one that did not is behind it by the gap.
	restoreCtx, cancelRestore := context.WithTimeout(ctx, 3*time.Minute)
	defer cancelRestore()
	before := time.Now()
	restored, err := n.client.RestoreMachine(restoreCtx, &v1alpha2.RestoreMachineRequest{
		MachineId: "copy", RequestId: "restore-copy", SnapshotId: "snap", NetworkPolicy: policy(),
	})
	require.NoError(t, err)
	require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, restored.Machine.State)
	require.Len(t, restored.Machine.Addresses, 1, "the node serves IPv4 only")
	newAddress := restored.Machine.Addresses[0]
	require.NotEqual(t, originAddress, newAddress, "the restored Machine has an address of its own, which the control below tells from the origin's")

	guest := n.guestClock(t, ctx, "copy", "copy-now")
	after := time.Now()
	assert.GreaterOrEqual(t, guest.Sub(originClock), gap, "the control: the guest's clock moved past the origin's last reading by the gap, so it is not the snapshot's")
	assert.False(t, guest.Before(before), "the guest's clock %s is %s behind the host's reading before the restore", guest, before.Sub(guest))
	assert.False(t, guest.After(after), "the guest's clock %s is %s ahead of the host's reading after the exec", guest, guest.Sub(after))

	assert.Equal(t, "connected", n.guestDial(t, ctx, "copy", "dial-host", target), "the restored guest reaches the address its policy allows")
	require.Eventually(t, func() bool { return listener.connections(newAddress) == 1 }, 10*time.Second, 100*time.Millisecond,
		"the listener saw one connection from the restored Machine's new address %s", newAddress)
	assert.Zero(t, listener.connections(originAddress), "the control: the dial did not leave from the origin's address %s, the one the snapshot's guest holds", originAddress)
}
