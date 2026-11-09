package firecracker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"runtime"
	"syscall"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Boot implements machine.Driver: it configures the started VMM from the boot
// description it is given, and starts the guest.
func (d *Driver) Boot(ctx context.Context, id machine.MachineID, spec machine.BootSpec) error {
	sock, err := d.socket(id, apiSocketName)
	if err != nil {
		return err
	}
	if err := await(ctx, sock); err != nil {
		return err
	}
	c := newClient(sock)

	var v struct {
		Version string `json:"firecracker_version"`
	}
	if err := c.do(ctx, "GET", "/version", nil, &v); err != nil {
		return err
	}
	if v.Version != Version {
		return fmt.Errorf("the VMM is Firecracker %s, not %s", v.Version, Version)
	}

	type put struct {
		path string
		body any
	}
	steps := []put{
		{"/machine-config", map[string]any{"vcpu_count": spec.Resources.VCPUs, "mem_size_mib": spec.Resources.MemoryBytes / mib}},
		{"/boot-source", map[string]any{"kernel_image_path": machine.InKernel, "initrd_path": machine.InInitrd, "boot_args": bootArgs(runtime.GOARCH)}},
	}
	for i, dev := range spec.Devices {
		// Writeback makes the guest's flushes reach the file; the default
		// ignores them.
		drive := map[string]any{
			"drive_id": fmt.Sprintf("disk%d", i), "path_on_host": machine.InDisk(i),
			"is_root_device": false, "is_read_only": dev.ReadOnly,
		}
		if !dev.ReadOnly {
			drive["cache_type"] = "Writeback"
		}
		steps = append(steps, put{"/drives/" + fmt.Sprintf("disk%d", i), drive})
	}
	if spec.Network.Tap != "" {
		steps = append(steps, put{"/network-interfaces/eth0", map[string]any{
			"iface_id": "eth0", "host_dev_name": spec.Network.Tap, "guest_mac": spec.Network.MAC,
		}})
	}
	steps = append(steps,
		put{"/vsock", map[string]any{"guest_cid": guestCID, "uds_path": inRun(vsockSocketName)}},
		put{"/actions", map[string]any{"action_type": "InstanceStart"}},
	)
	for _, s := range steps {
		if err := c.do(ctx, "PUT", s.path, s.body, nil); err != nil {
			return err
		}
	}
	return nil
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
	if runtime.GOARCH != "amd64" {
		return fmt.Errorf("%w: Firecracker v1.17.0 sends Ctrl-Alt-Del on x86_64 only", machine.ErrUnsupported)
	}
	sock, err := d.socket(id, apiSocketName)
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
		conn, err := (&net.Dialer{}).DialContext(ctx, "unix", sock)
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
