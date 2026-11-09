package firecracker

import (
	"context"
	"io"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmconsole"
)

// Console implements machine.Driver: a connection to the console socket the
// supervisor serves beside the run directory (ADR 0020), which is where the
// guest's serial console goes (Firecracker's own log goes to firecracker.log).
func (d *Driver) Console(ctx context.Context, id machine.MachineID) (io.ReadWriteCloser, error) {
	run, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return nil, err
	}
	return vmconsole.Dial(ctx, run)
}

// ConsoleLog implements machine.Driver: what the supervisor captured of the
// VMM's standard output in the run directory, which is the guest's serial
// console.
func (d *Driver) ConsoleLog(ctx context.Context, id machine.MachineID) (io.ReadCloser, error) {
	run, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return nil, err
	}
	return vmconsole.OpenLog(ctx, run)
}
