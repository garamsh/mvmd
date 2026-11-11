//go:build cloud_hypervisor_real

package cloudhypervisor_test

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/cloudhypervisor"
	"github.com/garamsh/mvmd/daemon/internal/drivertest"
	"github.com/garamsh/mvmd/daemon/internal/guestchannel"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// These tests are the scenarios of drivertest, which every Driver is held to,
// run against the pinned Cloud Hypervisor on the pinned kernel with the Guest
// Agent's initrd, as the user running them: no supervisor, no confinement
// beyond a root with the files at their fixed paths (drivertest), and no
// tap, so they need /dev/kvm and no root. They run through
// `make test-cloud-hypervisor`, which fetches the inputs and names them; a run
// without them fails, since a pass that never booted a guest would say
// nothing about the Driver. The scenarios of capabilities the Driver does not
// report pass by its refusing the operation.

func input(t *testing.T, name string) string {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Fatalf("%s is not set: run these tests through `make test-cloud-hypervisor`, which fetches the inputs and sets it", name)
	}
	return v
}

// subject is Cloud Hypervisor as drivertest runs a Driver.
func subject(t *testing.T) drivertest.Subject {
	t.Helper()
	return drivertest.Subject{
		Driver: func(t *testing.T, dirs drivertest.RunDirs) machine.Driver {
			d, err := cloudhypervisor.New(cloudhypervisor.Config{
				Binary: input(t, "MVMD_CLOUD_HYPERVISOR"), BinarySHA256: input(t, "MVMD_CLOUD_HYPERVISOR_SHA256"),
				Kernel: input(t, "MVMD_KERNEL"), KernelSHA256: input(t, "MVMD_KERNEL_SHA256"),
				Initrd: input(t, "MVMD_INITRD"), InitrdSHA256: input(t, "MVMD_INITRD_SHA256"),
				Dirs: dirs,
			})
			require.NoError(t, err)
			return d
		},
		Guest: func(d machine.Driver) machine.Guest { return guestchannel.NewGuest(d) },
		// What the supervisor does, without its confinement: run the described
		// process in a root holding the files at their fixed paths, with its event
		// pipe.
		Start: func(t *testing.T, p machine.ProcessDescription, s drivertest.Streams) drivertest.Process {
			return drivertest.StartInRoot(t, p, s)
		},
		KernelDigest: input(t, "MVMD_KERNEL_SHA256"),
		// A snapshot is config.json, state.json and memory-ranges. The VMM opens
		// the initrd its configuration names before it restores (ADR 0027), and
		// the restore is its command line, so it loads before Restore is called.
		Snapshot: drivertest.SnapshotFacts{Files: 3, RestoreBootFiles: []string{"/boot/initrd"}},
		Check:    checkVMM,
	}
}

func TestReal_ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect(t *testing.T) {
	drivertest.ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect(t, subject(t))
}

func TestReal_ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn(t *testing.T) {
	drivertest.ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn(t, subject(t))
}

func TestReal_ARestoredMachineHoldsTheDiskItsSnapshotRecordedAsAHotPluggedDrive(t *testing.T) {
	drivertest.ARestoredMachineHoldsTheDiskItsSnapshotRecordedAsAHotPluggedDrive(t, subject(t))
}

func TestReal_ADiskIsAttachedToARunningMachineAndDetachedAgain(t *testing.T) {
	drivertest.ADiskIsAttachedToARunningMachineAndDetachedAgain(t, subject(t))
}

func TestReal_AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug(t *testing.T) {
	drivertest.AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug(t, subject(t))
}

func TestReal_AMachineIsResizedWhileRunningAndItsGuestSeesTheNewSize(t *testing.T) {
	drivertest.AMachineIsResizedWhileRunningAndItsGuestSeesTheNewSize(t, subject(t))
}
