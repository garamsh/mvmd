//go:build e2e_real

package e2e_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const (
	firstBootID  = "11111111-1111-4111-8111-111111111111\n"
	secondBootID = "22222222-2222-4222-8222-222222222222\n"
	// quietFor is how long a Machine is watched to stay Stopped: several looks of the daemon for
	// a Machine to restart, which it makes once a second.
	quietFor = 6 * time.Second
)

// A Machine created with restart_on_boot that was Running when its host went down runs again when
// the deadline is first armed in the new boot, a real VMM with its data disk as it left it, and a
// Machine without the field stays Stopped, its reason naming the VMM's end. The same ending in the
// same boot restarts neither, with the deadline armed and so no refusal in the way. The host's
// boot is changed by giving the daemon a boot identity file of the test's in a mount namespace of
// its own.
func TestE2E_AMachineWithRestartOnBootRunsAgainWhenTheHostsBootChanges(t *testing.T) {
	const diskBytes = 32 << 20
	token := randomToken(t)
	n := startNodeWith(t, withOSFormBootSet(osFormSet), withBootID(firstBootID))
	n.arm(t)
	pulled := n.pullBootable(t, "plain")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	_, err := n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "boot-ro", RequestId: "create-boot-ro", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4})
	require.NoError(t, err)
	_, err = n.client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "boot-rw", RequestId: "create-boot-rw", SizeBytes: diskBytes, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	created, err := n.client.CreateMachine(ctx, &v1alpha2.CreateMachineRequest{
		MachineId:     "kept",
		RequestId:     "create-kept",
		Resources:     &v1alpha2.Resources{CpuCount: 1, MemoryBytes: 256 << 20},
		OsBoot:        &v1alpha2.OsBoot{BootSet: osFormSet, KernelCmdline: "mvmd.token=" + token},
		Disks:         []*v1alpha2.DiskAttachment{{DiskId: "boot-ro", ReadOnly: true}, {DiskId: "boot-rw"}},
		RestartOnBoot: true,
	})
	require.NoError(t, err)
	require.True(t, created.Machine.RestartOnBoot, "the OS-form Machine is created with restart_on_boot")
	created, err = n.client.CreateMachine(ctx, createRequest("plain", pulled.Digest))
	require.NoError(t, err)
	require.False(t, created.Machine.RestartOnBoot, "the image-form Machine is created without it")

	startBoth := func() {
		for _, id := range []string{"kept", "plain"} {
			stop := make(chan struct{})
			go pollSupervisorArgs(t, stop)
			started, err := n.client.StartMachine(ctx, &v1alpha2.StartMachineRequest{MachineId: id})
			close(stop)
			if err != nil {
				n.tmpDiag(t, id)
			}
			require.NoError(t, err)
			require.Equal(t, v1alpha2.MachineState_MACHINE_STATE_RUNNING, started.Machine.State, id)
		}
	}
	// hostEnds is what a host going down does to the node: the daemon, every supervisor and every
	// VMM end.
	hostEnds := func() {
		n.stopDaemon()
		n.killSupervisorsAndVMMs()
		n.waitVMMGone(t, "kept")
		n.waitVMMGone(t, "plain")
	}
	stoppedFor := func(why string) {
		t.Helper()
		for _, id := range []string{"kept", "plain"} {
			n.waitState(t, ctx, id, v1alpha2.MachineState_MACHINE_STATE_STOPPED)
		}
		time.Sleep(quietFor)
		assert.Equal(t, map[string]v1alpha2.MachineState{"kept": v1alpha2.MachineState_MACHINE_STATE_STOPPED, "plain": v1alpha2.MachineState_MACHINE_STATE_STOPPED},
			n.listed(t, ctx), why)
		assert.Empty(t, n.vmmPIDs(t, "kept"), why)
		assert.Empty(t, n.vmmPIDs(t, "plain"), why)
	}

	startBoth()
	assert.Equal(t, osFormReport{Token: token, ReadOnly0: "1", ReadOnly1: "0", Magic: "53ef", ROWrite: "refused", Before: "empty", Written: token}, n.osFormReport(t, ctx, "kept"),
		"the first boot of the OS-form Machine finds its data disk empty and writes the token")

	// Control: the host's boot is the same, and the deadline is armed, so a start would be permitted.
	hostEnds()
	n.startDaemon()
	require.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_ARMED, n.deadline(t).Phase, "the deadline the first daemon armed holds in the same boot")
	stoppedFor("the VMMs ended in the boot they started in, and neither Machine is started again")

	startBoth()
	assert.Equal(t, osFormReport{Token: token, ReadOnly0: "1", ReadOnly1: "0", Magic: "53ef", ROWrite: "refused", Before: token, Written: "none"}, n.osFormReport(t, ctx, "kept"),
		"a start the client asked for boots on the disk the first boot wrote")

	// The host's boot changes.
	hostEnds()
	n.setBootID(secondBootID)
	n.startDaemon()
	require.Equal(t, v1alpha2.DeadlinePhase_DEADLINE_PHASE_NOT_ARMED, n.deadline(t).Phase, "no deadline is armed in the new boot")
	stoppedFor("no Machine is started before the deadline is armed")

	n.renew(t, time.Hour, 1)
	n.waitState(t, ctx, "kept", v1alpha2.MachineState_MACHINE_STATE_RUNNING)
	assert.Equal(t, osFormReport{Token: token, ReadOnly0: "1", ReadOnly1: "0", Magic: "53ef", ROWrite: "refused", Before: token, Written: "none"}, n.osFormReport(t, ctx, "kept"),
		"the OS-form Machine is Running again, its console is the new boot's, and its disk holds what the first boot wrote")
	assert.NotEmpty(t, n.vmmPIDs(t, "kept"), "its VMM runs")

	plain, err := n.client.InspectMachine(ctx, &v1alpha2.InspectMachineRequest{MachineId: "plain"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha2.MachineState_MACHINE_STATE_STOPPED, plain.Machine.State, "the Machine without restart_on_boot stays Stopped")
	assert.Contains(t, plain.Machine.LastFailureReason, "the VMM ended", "its reason names the VMM's end")
	assert.Empty(t, n.vmmPIDs(t, "plain"))
}

// withBootID gives the daemon a boot identity file of the test's, which it reads in place of the
// host's, and keeps its path so that the test can rewrite it.
func withBootID(id string) nodeOption {
	return func(n *node, _ map[string]any) {
		n.bootIDFile = filepath.Join(n.work, "boot_id")
		n.setBootID(id)
	}
}

// setBootID replaces the content of the boot identity file in place, so that a daemon started
// after it reads the new identity.
func (n *node) setBootID(id string) {
	n.t.Helper()
	require.NoError(n.t, os.WriteFile(n.bootIDFile, []byte(id), 0o644))
}

// killSupervisorsAndVMMs ends every process of the supervisors' cgroup and of the Machines' VMMs'
// with SIGKILL, each by its pid.
func (n *node) killSupervisorsAndVMMs() {
	for _, root := range n.cgroups[:2] {
		_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || d.Name() != "cgroup.procs" {
				return nil
			}
			raw, _ := os.ReadFile(p)
			for _, f := range strings.Fields(string(raw)) {
				if pid, err := strconv.Atoi(f); err == nil {
					_ = syscall.Kill(pid, syscall.SIGKILL)
				}
			}
			return nil
		})
	}
}
