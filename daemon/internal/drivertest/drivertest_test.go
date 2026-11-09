package drivertest

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// A Driver that reports no capability is held to refusing what needs one. The
// stub is the in-memory Driver, which accepts every call, with the operations
// of the capabilities it does not report refused as a real Driver refuses them.
type refusing struct{ *memory.Driver }

var errRefused = errors.New("not supported by this Driver")

func (refusing) Snapshot(context.Context, machine.MachineID) (machine.DriverSnapshot, error) {
	return machine.DriverSnapshot{}, errRefused
}

func (refusing) DescribeRestore(context.Context, machine.MachineID, machine.BootSpec, []string) (machine.ProcessDescription, error) {
	return machine.ProcessDescription{}, errRefused
}

func (refusing) Restore(context.Context, machine.MachineID, machine.BootSpec) error {
	return errRefused
}

func (refusing) AttachDisk(context.Context, machine.MachineID, machine.BlockDevice) error {
	return errRefused
}

func (refusing) DetachDisk(context.Context, machine.MachineID, machine.DiskID) error {
	return errRefused
}

func (refusing) Drive(context.Context, machine.MachineID, machine.DiskID) (machine.DriveState, error) {
	return machine.DriveAbsent, errRefused
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

// stubSubject runs the scenarios over a Driver that reports no capability.
func stubSubject(driver func() machine.Driver) Subject {
	return Subject{
		Driver: func(*testing.T, RunDirs) machine.Driver { return driver() },
		Guest: func(d machine.Driver) machine.Guest {
			return pausedGuest{memory.NewGuest(0), d.(refusing).Driver}
		},
		Start: func(*testing.T, machine.ProcessDescription, Streams) Process {
			exited := make(chan struct{})
			close(exited)
			return stubProcess{exited}
		},
	}
}

func noCapabilities() *memory.Driver {
	d := memory.NewDriver(machine.DriverInfo{Name: "stub"})
	d.WithoutConsole()
	return d
}

func TestScenarios_ADriverThatReportsNoCapabilityIsHeldToRefusingItsOperations(t *testing.T) {
	s := stubSubject(func() machine.Driver { return refusing{noCapabilities()} })
	require.Empty(t, noCapabilities().Info().Capabilities, "control: the stub reports no capability")

	t.Run("booted", func(t *testing.T) { ABootedGuestAnswersThroughGuestConnAndItsStateChangesTakeEffect(t, s) })
	t.Run("snapshot", func(t *testing.T) { ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn(t, s) })
	t.Run("disks", func(t *testing.T) { ADiskIsAttachedToARunningMachineAndDetachedAgain(t, s) })
	t.Run("guest disks", func(t *testing.T) { AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug(t, s) })
}

func TestSnapshotOperationsAccepted_NamesTheOnesADriverAccepts(t *testing.T) {
	ctx := context.Background()
	spec := machine.BootSpec{}
	assert.Equal(t, []string{"Snapshot", "DescribeRestore", "Restore"},
		snapshotOperationsAccepted(ctx, noCapabilities(), "m1", spec), "a Driver that accepts them all")
	assert.Empty(t, snapshotOperationsAccepted(ctx, refusing{noCapabilities()}, "m1", spec))
}

func TestDiskOperationsAccepted_NamesTheOnesADriverAccepts(t *testing.T) {
	ctx := context.Background()
	dev := machine.BlockDevice{Disk: "hot"}
	assert.Equal(t, []string{"AttachDisk", "DetachDisk", "Drive"},
		diskOperationsAccepted(ctx, noCapabilities(), "m1", dev), "a Driver that accepts them all")
	assert.Empty(t, diskOperationsAccepted(ctx, refusing{noCapabilities()}, "m1", dev))
}
