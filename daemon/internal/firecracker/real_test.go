//go:build firecracker_real

package firecracker_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/drivertest"
	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/guestchannel"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// These tests are the scenarios of drivertest, which every Driver is held to,
// run against the pinned Firecracker on the pinned kernel with the Guest
// Agent's initrd, as the user running them: no supervisor, no confinement
// beyond a root with the files at their fixed paths (drivertest), and no
// tap, so they need /dev/kvm and no root. They run through
// `make test-firecracker`, which fetches the inputs and names them; a run
// without them fails, since a pass that never booted a guest would say
// nothing about the Driver.

func input(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set: run these tests through `make test-firecracker`, which fetches the inputs and sets it", name)
	}
	return v
}

// process is the VMM started in its root.
type process struct {
	cmd    *exec.Cmd
	exited chan struct{}
}

func (p process) Kill() error             { return p.cmd.Process.Kill() }
func (p process) Exited() <-chan struct{} { return p.exited }

// subject is Firecracker as drivertest runs a Driver. Its own check is the
// VMM's drive list (real_disks_test.go).
func subject(t *testing.T) drivertest.Subject {
	t.Helper()
	return drivertest.Subject{
		Driver: func(t *testing.T, dirs drivertest.RunDirs) machine.Driver {
			d, err := firecracker.New(firecracker.Config{
				Binary: input(t, "MVMD_FIRECRACKER"), BinarySHA256: input(t, "MVMD_FIRECRACKER_SHA256"),
				Kernel: input(t, "MVMD_KERNEL"), KernelSHA256: input(t, "MVMD_KERNEL_SHA256"),
				Initrd: input(t, "MVMD_INITRD"), InitrdSHA256: input(t, "MVMD_INITRD_SHA256"),
				Dirs: dirs,
			})
			require.NoError(t, err)
			return d
		},
		Guest: func(d machine.Driver) machine.Guest { return guestchannel.NewGuest(d) },
		// What the supervisor does, without its confinement: run the described
		// process in a root holding the files at their fixed paths.
		Start: func(t *testing.T, p machine.ProcessDescription, s drivertest.Streams) drivertest.Process {
			cmd, err := drivertest.InRoot(p)
			require.NoError(t, err)
			if s.In != nil {
				cmd.Stdin = s.In
			}
			cmd.Stdout, cmd.Stderr = s.Out, s.Err
			require.NoError(t, cmd.Start())
			exited := make(chan struct{})
			go func() { _ = cmd.Wait(); close(exited) }()
			return process{cmd, exited}
		},
		KernelDigest: input(t, "MVMD_KERNEL_SHA256"),
		// A snapshot is a state file and a memory file. Its VMM opens no boot file
		// on restore, and loads the snapshot when Restore asks it to.
		Snapshot: drivertest.SnapshotFacts{Files: 2, LoadsOnRestore: true},
		Check:    checkVMM,
	}
}

func TestReal_ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect(t *testing.T) {
	drivertest.ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect(t, subject(t))
}

func TestReal_ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn(t *testing.T) {
	drivertest.ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn(t, subject(t))
}

func TestReal_ADiskIsAttachedToARunningMachineAndDetachedAgain(t *testing.T) {
	drivertest.ADiskIsAttachedToARunningMachineAndDetachedAgain(t, subject(t))
}

func TestReal_AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug(t *testing.T) {
	drivertest.AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug(t, subject(t))
}
