package machine

import (
	"context"
	"time"
)

// Capability is an optional operation a Driver may support.
type Capability string

const (
	// CapabilityDiskHotplug attaches and detaches disks of a Running or
	// Paused Machine.
	CapabilityDiskHotplug Capability = "disk-hotplug"
	// CapabilityResize changes the CPU and memory of a Running or Paused
	// Machine.
	CapabilityResize Capability = "resize"
)

// DriverInfo names the Driver in service and what it supports.
type DriverInfo struct {
	Name         string
	Version      string
	Capabilities []Capability
}

// BlockDevice is a raw disk file handed to a VMM.
type BlockDevice struct {
	Disk     DiskID
	Path     string
	ReadOnly bool
}

// NetworkDevice is a tap's name and the guest MAC; the Driver learns no
// address and no policy.
type NetworkDevice struct {
	Tap string
	MAC string
}

// BootSpec is what a Driver needs from the lifecycle to boot a Machine.
// Devices are in boot order: the image, the root disk, then attached disks.
type BootSpec struct {
	Devices   []BlockDevice
	Network   NetworkDevice
	Resources Resources
}

// ProcessDescription is the VMM process a Driver describes for the
// supervisor to start and confine. It grants no capability, chooses no user
// and skips no step (ADR 0015).
type ProcessDescription struct {
	Executable string
	Args       []string
	// BootFiles are the files of the boot description, with their modes.
	BootFiles []BlockDevice
	// ReadFiles are host files the VMM reads beyond the boot files.
	ReadFiles []string
	// DeviceNodes are the device nodes the VMM opens.
	DeviceNodes []string
	// TapByDescriptor is true when the VMM takes its tap by descriptor.
	TapByDescriptor bool
	Overhead        ProcessOverhead

	// Tap and Guest are the lifecycle's, never the Driver's: For overwrites
	// them from the Machine, so a Driver cannot misstate what the cgroup's
	// limits are computed from.
	//
	// Tap is the Machine's tap, empty for a VMM with no network device. The
	// supervisor opens it and passes it as descriptor 3 when TapByDescriptor is
	// set; a VMM that opens it by name shares the host's network namespace, and
	// every other VMM gets one of its own.
	Tap string
	// Guest is the guest's memory and vCPUs, which with Overhead fix the
	// VMM's cgroup limits.
	Guest Resources
}

// For returns the description with the parts of it that come from the
// Machine's boot rather than from the Driver (ADR 0015).
func (p ProcessDescription) For(boot BootSpec) ProcessDescription {
	p.Tap, p.Guest = boot.Network.Tap, boot.Resources
	return p
}

// ProcessOverhead is what the VMM's cgroup limits are set from (ADR 0015).
// MemoryBytes and CPUs are added to the guest's memory and vCPUs. Tasks is not:
// it is the whole task count for this Machine's vCPUs and devices, which the
// Driver computes from the BootSpec and which becomes pids.max as it is.
type ProcessOverhead struct {
	MemoryBytes uint64
	Tasks       uint32
	CPUs        float64
}

// Driver is the conversation with a virtual machine monitor. It describes the
// VMM process and drives the VMM once the supervisor has started it; it never
// starts the process itself.
type Driver interface {
	// Info names the Driver and the optional operations it supports.
	Info() DriverInfo
	// Describe states the VMM process for a Machine's start.
	Describe(ctx context.Context, id MachineID, spec BootSpec) (ProcessDescription, error)
	// Boot configures the started VMM and boots the guest.
	Boot(ctx context.Context, id MachineID) error
	// Shutdown asks the guest to stop and waits up to grace for it to do so.
	Shutdown(ctx context.Context, id MachineID, grace time.Duration) error
	Pause(ctx context.Context, id MachineID) error
	Resume(ctx context.Context, id MachineID) error
	// AttachDisk and DetachDisk need CapabilityDiskHotplug.
	AttachDisk(ctx context.Context, id MachineID, dev BlockDevice) error
	DetachDisk(ctx context.Context, id MachineID, disk DiskID) error
	// Resize needs CapabilityResize.
	Resize(ctx context.Context, id MachineID, target Resources) error
	// Observe sets the function the Driver calls for each event it raises on
	// its own, from any goroutine. NewService calls it once.
	Observe(report func(DriverEvent))
}

func (i DriverInfo) has(c Capability) bool {
	for _, have := range i.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}
