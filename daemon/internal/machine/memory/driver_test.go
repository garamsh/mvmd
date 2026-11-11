package memory_test

import (
	"context"
	"errors"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

func TestGuestConn_ReturnsErrVMMEndedAndErrGuestUnreachableWhenNoGuestServes(t *testing.T) {
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})

	_, err := d.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
	require.ErrorIs(t, err, machine.ErrVMMEnded)
}

func TestGuestConn_ReturnsErrGuestUnreachableWhenBootedAndNoGuestServes(t *testing.T) {
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})
	require.NoError(t, d.Boot(context.Background(), "m1", machine.BootSpec{}))

	_, err := d.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
	assert.NotErrorIs(t, err, machine.ErrVMMEnded)
}

func TestGuestConn_ReturnsErrVMMEndedAndErrGuestUnreachableWhenShutdownAndNoGuestServes(t *testing.T) {
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})
	require.NoError(t, d.Boot(context.Background(), "m1", machine.BootSpec{}))
	require.NoError(t, d.Shutdown(context.Background(), "m1", 0))

	_, err := d.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
	require.ErrorIs(t, err, machine.ErrVMMEnded)
}

func TestGuestConn_ReturnsErrGuestUnreachableWhenInjectedFailure(t *testing.T) {
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})
	d.FailAt("guest-conn", errors.New("the machine is not running"))

	_, err := d.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
	assert.Contains(t, err.Error(), "the machine is not running")
}

func TestGuestConn_SucceedsWhenTheGuestServes(t *testing.T) {
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})
	d.ServeGuest("m1", machine.GuestAgentPort, func(guest net.Conn) {
		_ = guest.Close()
	})

	conn, err := d.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.NoError(t, err)
	require.NotNil(t, conn)
	_ = conn.Close()
}

func TestDrive_ARestoredVMMHoldsARecordedDiskAsHotPluggedAndItsOwnDevicesAsGivenAtBoot(t *testing.T) {
	ctx := context.Background()
	spec := machine.BootSpec{
		Devices:       []machine.BlockDevice{{Path: "/image"}, {Disk: "root", Path: "/root"}},
		RecordedDisks: []machine.BlockDevice{{Disk: "data", Path: "/data"}},
	}
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})
	require.NoError(t, d.Restore(ctx, "m", spec))

	for disk, want := range map[machine.DiskID]machine.DriveState{
		"data": machine.DriveHotPlugged, "root": machine.DriveAtBoot, "none": machine.DriveAbsent,
	} {
		got, err := d.Drive(ctx, "m", disk)
		require.NoError(t, err, disk)
		assert.Equal(t, want, got, disk)
	}
}

func TestDrive_ABootedVMMHoldsEveryDeviceItsDescriptionNamesAsGivenAtBoot(t *testing.T) {
	ctx := context.Background()
	spec := machine.BootSpec{Devices: []machine.BlockDevice{{Path: "/image"}, {Disk: "root", Path: "/root"}, {Disk: "data", Path: "/data"}}}
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})
	require.NoError(t, d.Boot(ctx, "m", spec))

	got, err := d.Drive(ctx, "m", "data")
	require.NoError(t, err)
	assert.Equal(t, machine.DriveAtBoot, got, "control: a device of a boot is given at boot; only a restore's recorded disk is not")
}
