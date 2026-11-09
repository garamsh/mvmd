package cloudhypervisor

import (
	"context"
	"net"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vsockdial"
)

// GuestConn implements machine.Driver: it connects to the vsock socket in the
// run directory and asks Cloud Hypervisor for the guest's port (vsockdial).
func (d *Driver) GuestConn(ctx context.Context, id machine.MachineID, port uint32) (net.Conn, error) {
	sock, err := d.socket(id, vsockSocketName)
	if err != nil {
		return nil, err
	}
	return vsockdial.Dial(ctx, sock, port)
}
