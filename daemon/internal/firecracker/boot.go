package firecracker

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"syscall"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
)

// Boot implements machine.Driver: it configures the started VMM from the boot
// description it is given, and starts the guest.
func (d *Driver) Boot(ctx context.Context, id machine.MachineID, spec machine.BootSpec) error {
	c, err := d.started(ctx, id)
	if err != nil {
		return err
	}

	type put struct {
		path string
		body any
	}
	steps := []put{
		{"/machine-config", map[string]any{"vcpu_count": spec.Resources.VCPUs, "mem_size_mib": spec.Resources.MemoryBytes / mib}},
		{"/boot-source", map[string]any{"kernel_image_path": machine.InKernel, "initrd_path": machine.InInitrd, "boot_args": kernelArgs(runtime.GOARCH, spec)}},
	}
	for i, dev := range spec.Devices {
		name := diskdevice.Name(i, dev.Disk)
		steps = append(steps, put{"/drives/" + name, drive(name, machine.InDisk(i), dev.ReadOnly)})
	}
	if spec.Network.Tap != "" {
		steps = append(steps, put{"/network-interfaces/" + netIface, map[string]any{
			"iface_id": netIface, "host_dev_name": spec.Network.Tap, "guest_mac": spec.Network.MAC,
		}})
	}
	steps = append(steps,
		put{"/vsock", map[string]any{"guest_cid": vmmconfig.GuestCID, "uds_path": vmmconfig.InRun(vmmconfig.VsockSocket)}},
		put{"/actions", map[string]any{"action_type": "InstanceStart"}},
	)
	for _, s := range steps {
		if err := c.do(ctx, "PUT", s.path, s.body, nil); err != nil {
			return err
		}
	}
	return nil
}

// started waits for the VMM's API socket to accept a connection and refuses a
// VMM that is not the release this Driver is written against.
func (d *Driver) started(ctx context.Context, id machine.MachineID) (*client, error) {
	sock, err := vmmconfig.RunPath(d.cfg.Dirs, id, vmmconfig.APISocket)
	if err != nil {
		return nil, err
	}
	if err := unixsock.Await(ctx, sock); err != nil {
		return nil, err
	}
	c := newClient(sock)
	var v struct {
		Version string `json:"firecracker_version"`
	}
	if err := c.do(ctx, "GET", "/version", nil, &v); err != nil {
		return nil, err
	}
	if v.Version != Version {
		return nil, fmt.Errorf("the VMM is Firecracker %s, not %s", v.Version, Version)
	}
	return c, nil
}

// Pause implements machine.Driver.
func (d *Driver) Pause(ctx context.Context, id machine.MachineID) error {
	return d.setState(ctx, id, "Paused")
}

// Resume implements machine.Driver.
func (d *Driver) Resume(ctx context.Context, id machine.MachineID) error {
	return d.setState(ctx, id, "Resumed")
}

func (d *Driver) setState(ctx context.Context, id machine.MachineID, state string) error {
	c, err := d.api(id)
	if err != nil {
		return err
	}
	return c.do(ctx, "PATCH", "/vm", map[string]string{"state": state}, nil)
}

// Shutdown implements machine.Driver. It sends Ctrl-Alt-Del, which a guest
// kernel with the i8042 and AT keyboard drivers turns into a reset that ends
// the VMM, and waits up to grace for the API socket to go away. A guest whose
// kernel has no such driver never answers, so the wait runs its whole grace.
func (d *Driver) Shutdown(ctx context.Context, id machine.MachineID, grace time.Duration) error {
	if d.goarch != "amd64" {
		return fmt.Errorf("%w: Firecracker v1.17.0 sends Ctrl-Alt-Del on x86_64 only", machine.ErrUnsupported)
	}
	sock, err := vmmconfig.RunPath(d.cfg.Dirs, id, vmmconfig.APISocket)
	if err != nil {
		return err
	}
	if err := newClient(sock).do(ctx, "PUT", "/actions", map[string]string{"action_type": "SendCtrlAltDel"}, nil); err != nil {
		return err
	}
	if grace <= 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, grace)
	defer cancel()
	for {
		conn, err := unixsock.DialContext(ctx, sock)
		if err != nil {
			switch {
			case ctx.Err() != nil:
				return fmt.Errorf("the guest did not stop within %s: %w", grace, errors.Join(ctx.Err(), err))
			case errors.Is(err, syscall.ENOENT), errors.Is(err, syscall.ECONNREFUSED):
				return nil // no socket, or none listening: the VMM has ended
			default:
				return fmt.Errorf("cannot tell whether the VMM has ended: %w", err)
			}
		}
		_ = conn.Close()
		select {
		case <-ctx.Done():
			return fmt.Errorf("the guest did not stop within %s: %w", grace, ctx.Err())
		case <-time.After(20 * time.Millisecond):
		}
	}
}
