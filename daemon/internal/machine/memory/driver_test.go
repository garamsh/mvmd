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

func TestGuestConn_ReturnsErrVMMEndedAndErrGuestUnreachableWhenInjectedFailure(t *testing.T) {
	d := memory.NewDriver(machine.DriverInfo{Name: "memory"})
	d.FailAt("guest-conn", errors.New("the machine is not running"))

	_, err := d.GuestConn(context.Background(), "m1", machine.GuestAgentPort)
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
	require.ErrorIs(t, err, machine.ErrVMMEnded)
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
