// Package guestaccess reaches a Machine's guest through its run directory:
// the serial console as a live session and as a log, and a port of the guest
// over the vsock socket. Both VMM Drivers embed Access, so the same three
// operations are the same code under each.
package guestaccess

import (
	"context"
	"io"
	"net"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmconsole"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
	"github.com/garamsh/mvmd/daemon/internal/vsockdial"
)

// Access reaches the guests of the Machines whose run directories Dirs names.
type Access struct {
	Dirs vmmconfig.RunDirs
}

// Console implements machine.Driver: a connection to the console socket the
// supervisor serves beside the run directory (ADR 0020), which is where the
// guest's serial console goes (the VMM's own log goes to a file of its own).
func (a Access) Console(ctx context.Context, id machine.MachineID) (io.ReadWriteCloser, error) {
	run, err := a.Dirs.RunDir(id)
	if err != nil {
		return nil, err
	}
	return vmconsole.Dial(ctx, run)
}

// ConsoleLog implements machine.Driver: what the supervisor captured of the
// VMM's standard output in the run directory, which is the guest's serial
// console.
func (a Access) ConsoleLog(ctx context.Context, id machine.MachineID) (io.ReadCloser, error) {
	run, err := a.Dirs.RunDir(id)
	if err != nil {
		return nil, err
	}
	return vmconsole.OpenLog(ctx, run)
}

// GuestConn implements machine.Driver: it connects to the vsock socket in the
// run directory and asks the VMM for the guest's port (vsockdial).
func (a Access) GuestConn(ctx context.Context, id machine.MachineID, port uint32) (net.Conn, error) {
	sock, err := vmmconfig.RunPath(a.Dirs, id, vmmconfig.VsockSocket)
	if err != nil {
		return nil, err
	}
	return vsockdial.Dial(ctx, sock, port)
}
