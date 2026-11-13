//go:build e2e_real

package e2e_test

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// TEMPORARY diagnostics.
func (n *node) tmpDiag(t *testing.T, id string) {
	d := n.stateDir + "/machines/" + id
	st := n.deadline(t)
	t.Logf("DIAG deadline %v remaining %v", st.Phase, st.Remaining.AsDuration())
	for _, c := range []string{"cat /proc/uptime", "ls -la " + d + "/run", "tail -c 1500 " + d + "/run/vmm.log", "tail -c 1500 " + d + "/run/firecracker.log", "tail -c 1500 " + d + "/run/cloud-hypervisor.log", "tail -c 800 " + d + "/supervisor.log"} {
		out, _ := exec.Command("sh", "-c", c+" 2>&1").CombinedOutput()
		t.Logf("DIAG %s [%s]:\n%s", id, c, out)
	}
}

// pollSupervisorArgs logs the supervisor command lines seen while stop is open.
func pollSupervisorArgs(t *testing.T, stop chan struct{}) {
	seen := map[string]bool{}
	for {
		select {
		case <-stop:
			return
		default:
		}
		out, _ := exec.Command("sh", "-c", "ps -eo args | grep expiry-boottime | grep -v grep; cat /proc/uptime").CombinedOutput()
		if !seen[string(out)] {
			seen[string(out)] = true
			t.Logf("DIAG supervisors: %s", out)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestE2E_TmpSameBootRestartWithoutNamespace(t *testing.T) {
	n := startNodeWith(t, withOSFormBootSet(osFormSet))
	n.arm(t)
	pulled := n.pullBootable(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()
	_, err := n.client.CreateMachine(ctx, createRequest("plain", pulled.Digest))
	require.NoError(t, err)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "plain"})
	require.NoError(t, err)
	n.stopDaemon()
	n.killSupervisorsAndVMMs()
	n.waitVMMGone(t, "plain")
	n.startDaemon()
	n.waitState(t, ctx, "plain", v1alpha2.MachineState_MACHINE_STATE_STOPPED)
	stop := make(chan struct{})
	go pollSupervisorArgs(t, stop)
	_, err = n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: "plain"})
	close(stop)
	if err != nil {
		n.tmpDiag(t, "plain")
	}
	require.NoError(t, err)
}
