package cloudhypervisor

import (
	"context"
	"fmt"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// resizeWait bounds how long Resize waits for the VMM to report the target. The
// guest takes part in a resize, so the VMM's call returns before it is done.
const resizeWait = 30 * time.Second

// resizePoll is how often Resize asks the VMM what the guest has reached.
const resizePoll = 50 * time.Millisecond

// resizeCapable reports whether this architecture has the hot-plug a resize
// needs: v54.0's vCPU and virtio-mem hot-plug are x86_64's (ADR 0029).
func resizeCapable(goarch string) bool { return goarch == "amd64" }

// Resize implements machine.Driver. It reads the VMM's configuration and sends
// PUT /vm.resize with the figures that differ from it, then waits until the VMM
// reports the target: the vCPUs it has configured, and the memory the guest has
// plugged (memory_actual_size), which is the guest's progress and not the
// request's. A call that finds both at the target sends nothing, so a retry
// after a failure converges; a wait that runs out is an error, the lifecycle's
// failed.
func (d *Driver) Resize(ctx context.Context, id machine.MachineID, target machine.Resources) error {
	if !resizeCapable(d.goarch) {
		return unsupported("resize on " + d.goarch)
	}
	if target.VCPUs == 0 || target.MemoryBytes == 0 {
		return fmt.Errorf("%w: %d vCPUs and %d bytes of memory is not a resize the VMM takes", machine.ErrInvalid, target.VCPUs, target.MemoryBytes)
	}
	c, err := d.api(id)
	if err != nil {
		return err
	}
	info, err := c.info(ctx)
	if err != nil {
		return err
	}
	body := map[string]uint64{}
	if info.Config.CPUs.BootVCPUs != target.VCPUs {
		body["desired_vcpus"] = uint64(target.VCPUs)
	}
	if info.configuredMemory() != target.MemoryBytes {
		body["desired_ram"] = target.MemoryBytes
	}
	if len(body) > 0 {
		if err := c.do(ctx, "PUT", "/vm.resize", body, nil); err != nil {
			return err
		}
	}
	return awaitResized(ctx, c, target)
}

// awaitResized polls the VMM until it reports the target.
func awaitResized(ctx context.Context, c *client, target machine.Resources) error {
	ctx, cancel := context.WithTimeout(ctx, resizeWait)
	defer cancel()
	for {
		info, err := c.info(ctx)
		if err != nil {
			return fmt.Errorf("wait for the guest to resize: %w", err)
		}
		if info.Config.CPUs.BootVCPUs == target.VCPUs && info.MemoryActualSize == target.MemoryBytes {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the VMM reports %d vCPUs and %d bytes of memory, not the %d and %d asked for: %w",
				info.Config.CPUs.BootVCPUs, info.MemoryActualSize, target.VCPUs, target.MemoryBytes, ctx.Err())
		case <-time.After(resizePoll):
		}
	}
}
