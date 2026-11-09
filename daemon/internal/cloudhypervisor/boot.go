package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"syscall"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/unixsock"
)

// Boot implements machine.Driver. The process Describe states was started with
// the VM's whole configuration on its command line, so Cloud Hypervisor creates
// and boots the VM itself: Boot waits for the API socket, refuses a VMM that is
// not the release this Driver is written against, and waits for the VM to be
// Running. The spec is not read: it is what Describe was given.
func (d *Driver) Boot(ctx context.Context, id machine.MachineID, _ machine.BootSpec) error {
	sock, err := d.socket(id, apiSocketName)
	if err != nil {
		return err
	}
	if err := unixsock.Await(ctx, sock); err != nil {
		return err
	}
	c := newClient(sock)
	var ping struct {
		Version string `json:"version"`
	}
	if err := c.do(ctx, "GET", "/vmm.ping", nil, &ping); err != nil {
		return err
	}
	// v54.0 reports "54.0.0": the release with its patch number.
	if v := strings.TrimPrefix(ping.Version, "v"); v != Version && !strings.HasPrefix(v, Version+".") {
		return fmt.Errorf("the VMM is Cloud Hypervisor %s, not %s", ping.Version, Version)
	}
	for wait := time.Millisecond; ; wait = min(2*wait, 50*time.Millisecond) {
		state, err := c.state(ctx)
		switch {
		case err == nil && state == "Running":
			return nil
		case err == nil && state != "Created":
			return fmt.Errorf("the VM is %s, not Running", state)
		case err != nil && !notYet(err):
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the VM did not reach Running: %w", ctx.Err())
		case <-time.After(wait):
		}
	}
}

// state is the VM's state, from GET /vm.info.
func (c *client) state(ctx context.Context) (string, error) {
	var info struct {
		State string `json:"state"`
	}
	if err := c.do(ctx, "GET", "/vm.info", nil, &info); err != nil {
		return "", err
	}
	return info.State, nil
}

// notYet reports whether the answer says only that the VM is not created yet:
// vm.info is a 404 until it is.
func notYet(err error) bool {
	var refused *apiError
	return errors.As(err, &refused) && refused.Status == http.StatusNotFound
}

// Pause implements machine.Driver.
func (d *Driver) Pause(ctx context.Context, id machine.MachineID) error {
	return d.put(ctx, id, "/vm.pause")
}

// Resume implements machine.Driver.
func (d *Driver) Resume(ctx context.Context, id machine.MachineID) error {
	return d.put(ctx, id, "/vm.resume")
}

func (d *Driver) put(ctx context.Context, id machine.MachineID, path string) error {
	c, err := d.api(id)
	if err != nil {
		return err
	}
	return c.do(ctx, "PUT", path, nil, nil)
}

// Shutdown implements machine.Driver. It presses the ACPI power button, which a
// guest kernel with the ACPI button driver and something to act on it turns
// into a power-off that ends the VMM, and waits up to grace for the API socket
// to go away. A guest that does nothing with it never answers, so the wait runs
// its whole grace.
func (d *Driver) Shutdown(ctx context.Context, id machine.MachineID, grace time.Duration) error {
	sock, err := d.socket(id, apiSocketName)
	if err != nil {
		return err
	}
	if err := newClient(sock).do(ctx, "PUT", "/vm.power-button", nil, nil); err != nil {
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
