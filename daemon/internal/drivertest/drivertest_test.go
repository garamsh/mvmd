package drivertest_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/drivertest"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// stub is the in-memory Driver, which accepts every call, standing for a Driver
// that reports no capability: the operations of the capabilities it does not
// report are refused as the port says, with an error wrapping ErrUnsupported.
// What it is told to do instead is the fault a control injects.
type stub struct {
	*memory.Driver
	// snapshotAccepted makes Snapshot succeed, as a Driver that does not check
	// the capability would; snapshotRefusal is the error it refuses with, which
	// is ErrUnsupported unless a control sets another cause.
	snapshotAccepted bool
	snapshotRefusal  error
	// resizeAccepted makes Resize succeed, as a Driver that does not check the
	// capability would.
	resizeAccepted bool
}

func (s stub) Resize(ctx context.Context, id machine.MachineID, target machine.Resources) error {
	if s.resizeAccepted {
		return s.Driver.Resize(ctx, id, target)
	}
	return machine.ErrUnsupported
}

func (s stub) Snapshot(ctx context.Context, id machine.MachineID) (machine.DriverSnapshot, error) {
	if s.snapshotAccepted {
		return s.Driver.Snapshot(ctx, id)
	}
	return machine.DriverSnapshot{}, s.snapshotRefusal
}

// Shutdown is what a guest whose kernel cannot take the request does to it: the grace runs out
// and the Driver reports that the guest did not stop.
func (stub) Shutdown(ctx context.Context, _ machine.MachineID, grace time.Duration) error {
	select {
	case <-time.After(grace):
		return fmt.Errorf("the guest did not stop within %s: %w", grace, context.DeadlineExceeded)
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (stub) DescribeRestore(context.Context, machine.MachineID, machine.BootSpec, []string) (machine.ProcessDescription, error) {
	return machine.ProcessDescription{}, machine.ErrUnsupported
}

func (stub) Restore(context.Context, machine.MachineID, machine.BootSpec) error {
	return machine.ErrUnsupported
}

func (stub) AttachDisk(context.Context, machine.MachineID, machine.BlockDevice) error {
	return machine.ErrUnsupported
}

func (stub) DetachDisk(context.Context, machine.MachineID, machine.DiskID) error {
	return machine.ErrUnsupported
}

func (stub) Drive(context.Context, machine.MachineID, machine.DiskID) (machine.DriveState, error) {
	return machine.DriveAbsent, machine.ErrUnsupported
}

func (stub) Console(context.Context, machine.MachineID) (io.ReadWriteCloser, error) {
	return nil, machine.ErrUnsupported
}

// pausedGuest is a guest that answers nothing while its Machine is paused, as a
// real one does: the in-memory Guest models no VMM.
type pausedGuest struct {
	*memory.Guest
	d *memory.Driver
}

func (g pausedGuest) Health(ctx context.Context, id machine.MachineID) error {
	if g.d.Paused(id) {
		return errors.New("paused")
	}
	return g.Guest.Health(ctx, id)
}

// stubProcess is a VMM that is already over: the stub Driver has none.
type stubProcess struct{ exited chan struct{} }

func (stubProcess) Kill() error               { return nil }
func (p stubProcess) Exited() <-chan struct{} { return p.exited }

func subject(s stub) drivertest.Subject {
	return drivertest.Subject{
		Driver: func(*testing.T, drivertest.RunDirs) machine.Driver { return s },
		Guest: func(machine.Driver) machine.Guest {
			// The in-memory guest models no VMM: it has the vCPUs the suite's
			// Machines boot with, as a resize that was refused leaves it.
			g := memory.NewGuest(0)
			g.PresentVcpus("m1", 2)
			return pausedGuest{g, s.Driver}
		},
		Start: func(*testing.T, machine.ProcessDescription, drivertest.Streams) drivertest.Process {
			exited := make(chan struct{})
			close(exited)
			return stubProcess{exited}
		},
	}
}

func newStub() stub {
	d := memory.NewDriver(machine.DriverInfo{Name: "stub"})
	d.WithoutConsole()
	return stub{Driver: d, snapshotRefusal: machine.ErrUnsupported}
}

// runScenarios runs the four scenarios, each over a stub of its own, since a
// Machine one leaves paused is not another's to boot.
func runScenarios(t *testing.T, fault func(*stub)) {
	each := func(run func(*testing.T, drivertest.Subject)) func(*testing.T) {
		return func(t *testing.T) {
			s := newStub()
			fault(&s)
			run(t, subject(s))
		}
	}
	t.Run("booted", each(drivertest.ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect))
	t.Run("snapshot", each(drivertest.ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn))
	t.Run("restored disks", each(drivertest.ARestoredMachineHoldsTheDiskItsSnapshotRecordedAsAHotPluggedDrive))
	t.Run("disks", each(drivertest.ADiskIsAttachedToARunningMachineAndDetachedAgain))
	t.Run("guest disks", each(drivertest.AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug))
	t.Run("resize", each(drivertest.AMachineIsResizedWhileRunningAndItsGuestSeesTheNewSize))
}

// controlEnv names the fault a child run of this test binary injects.
const controlEnv = "DRIVERTEST_CONTROL"

// A Driver that reports no capability is held to refusing what needs one, with
// the error the port names. The controls are the same stub with one fault: the
// scenarios, run in a child of this binary since a failure cannot be observed
// from inside the test that fails, must fail on it. So the pass above them is
// the stub's refusal, and not a check that accepts anything.
func TestScenarios_ADriverThatReportsNoCapabilityIsHeldToRefusingItsOperations(t *testing.T) {
	require.Empty(t, newStub().Info().Capabilities, "control: the stub reports no capability")

	switch os.Getenv(controlEnv) {
	case "accepts-snapshot":
		runScenarios(t, func(s *stub) { s.snapshotAccepted = true })
		return
	case "refuses-for-another-cause":
		runScenarios(t, func(s *stub) { s.snapshotRefusal = errors.New("the Machine is not running") })
		return
	case "accepts-resize":
		runScenarios(t, func(s *stub) { s.resizeAccepted = true })
		return
	}

	runScenarios(t, func(*stub) {})

	for _, c := range []struct{ name, fault, scenario, operation string }{
		{"accepts-snapshot", "a Driver that does not check the capability accepts Snapshot", "snapshot", "Snapshot"},
		{"refuses-for-another-cause", "a refusal that is not ErrUnsupported is not the refusal for the capability", "snapshot", "Snapshot"},
		{"accepts-resize", "a Driver that does not check the capability accepts Resize", "resize", "Resize"},
	} {
		t.Run("control "+c.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestScenarios_ADriverThatReportsNoCapabilityIsHeldToRefusingItsOperations$", "-test.v")
			cmd.Env = append(os.Environ(), controlEnv+"="+c.name)
			out, err := cmd.CombinedOutput()
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, "the scenarios pass though %s:\n%s", c.fault, out)
			assert.Contains(t, string(out), "--- FAIL: TestScenarios_ADriverThatReportsNoCapabilityIsHeldToRefusingItsOperations/"+c.scenario)
			assert.Contains(t, string(out), c.operation)
		})
	}
}
