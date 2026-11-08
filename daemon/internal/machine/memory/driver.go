package memory

import (
	"context"
	"sync"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Driver is an in-memory machine.Driver. Its steps are "describe", "boot",
// "shutdown", "pause", "resume", "attach", "detach" and "resize".
type Driver struct {
	Failures
	info machine.DriverInfo

	mu      sync.Mutex
	specs   map[machine.MachineID]machine.BootSpec
	grace   time.Duration
	devices map[machine.MachineID][]machine.DiskID
	sizes   map[machine.MachineID]machine.Resources
}

// NewDriver returns a Driver reporting the given name, version and
// capabilities.
func NewDriver(info machine.DriverInfo) *Driver {
	return &Driver{
		info:    info,
		specs:   make(map[machine.MachineID]machine.BootSpec),
		devices: make(map[machine.MachineID][]machine.DiskID),
		sizes:   make(map[machine.MachineID]machine.Resources),
	}
}

// Info implements machine.Driver.
func (d *Driver) Info() machine.DriverInfo { return d.info }

// Describe implements machine.Driver.
func (d *Driver) Describe(_ context.Context, id machine.MachineID, spec machine.BootSpec) (machine.ProcessDescription, error) {
	if err := d.enter("describe"); err != nil {
		return machine.ProcessDescription{}, err
	}
	d.mu.Lock()
	d.specs[id] = spec
	d.mu.Unlock()
	return machine.ProcessDescription{Executable: "/usr/libexec/" + d.info.Name, BootFiles: spec.Devices}, nil
}

// Boot implements machine.Driver.
func (d *Driver) Boot(_ context.Context, id machine.MachineID) error {
	return d.enter("boot")
}

// Shutdown implements machine.Driver.
func (d *Driver) Shutdown(_ context.Context, _ machine.MachineID, grace time.Duration) error {
	d.mu.Lock()
	d.grace = grace
	d.mu.Unlock()
	return d.enter("shutdown")
}

// Pause implements machine.Driver.
func (d *Driver) Pause(_ context.Context, _ machine.MachineID) error { return d.enter("pause") }

// Resume implements machine.Driver.
func (d *Driver) Resume(_ context.Context, _ machine.MachineID) error { return d.enter("resume") }

// AttachDisk implements machine.Driver.
func (d *Driver) AttachDisk(_ context.Context, id machine.MachineID, dev machine.BlockDevice) error {
	if err := d.enter("attach"); err != nil {
		return err
	}
	d.mu.Lock()
	d.devices[id] = append(d.devices[id], dev.Disk)
	d.mu.Unlock()
	return nil
}

// DetachDisk implements machine.Driver.
func (d *Driver) DetachDisk(_ context.Context, id machine.MachineID, disk machine.DiskID) error {
	if err := d.enter("detach"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := d.devices[id][:0]
	for _, have := range d.devices[id] {
		if have != disk {
			kept = append(kept, have)
		}
	}
	d.devices[id] = kept
	return nil
}

// Resize implements machine.Driver.
func (d *Driver) Resize(_ context.Context, id machine.MachineID, target machine.Resources) error {
	if err := d.enter("resize"); err != nil {
		return err
	}
	d.mu.Lock()
	d.sizes[id] = target
	d.mu.Unlock()
	return nil
}

// BootSpec returns the boot description the last Describe for the Machine got.
func (d *Driver) BootSpec(id machine.MachineID) machine.BootSpec {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.specs[id]
}

// Grace returns the grace period of the last Shutdown.
func (d *Driver) Grace() time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.grace
}

// HotDisks returns the disks the Driver attached to the running Machine.
func (d *Driver) HotDisks(id machine.MachineID) []machine.DiskID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]machine.DiskID(nil), d.devices[id]...)
}

// Resized returns the target of the last Resize of the Machine.
func (d *Driver) Resized(id machine.MachineID) machine.Resources {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.sizes[id]
}
