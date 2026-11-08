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
}

// ProcessOverhead is what the VMM needs beyond the guest's memory and vCPUs.
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
}

func (i DriverInfo) has(c Capability) bool {
	for _, have := range i.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}
