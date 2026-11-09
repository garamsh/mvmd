package machine

import (
	"context"
	"errors"
	"io"
	"net"
	"time"
)

// ErrConsoleUnavailable is a console, or the log of one, that a Driver cannot
// open.
var ErrConsoleUnavailable = errors.New("the console is not available")

// GuestAgentPort is the port the Guest Agent listens on in every guest
// (guest.md), the one a caller of Driver.GuestConn names for it.
const GuestAgentPort uint32 = 1024

// Capability is an optional operation a Driver may support.
type Capability string

const (
	// CapabilityDiskHotplug attaches and detaches disks of a Running or
	// Paused Machine.
	CapabilityDiskHotplug Capability = "disk-hotplug"
	// CapabilityResize changes the CPU and memory of a Running or Paused
	// Machine.
	CapabilityResize Capability = "resize"
	// CapabilitySnapshot snapshots a Running or Paused Machine and restores
	// a Machine from a snapshot.
	CapabilitySnapshot Capability = "snapshot"
)

// DriverInfo names the Driver in service and what it supports.
type DriverInfo struct {
	Name    string
	Version string
	// CPUModel is the host CPU model the Driver's VMM runs guests on, which a
	// snapshot is only restored on again.
	CPUModel     string
	Capabilities []Capability
}

// DriverSnapshot is what a Driver made of a paused Machine: the files it
// wrote in the run directory and the digest of the guest kernel the snapshot
// holds. It is the Driver's own and the inside does not read the files.
type DriverSnapshot struct {
	Files        []string
	KernelDigest string
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
	// RunDir is the Machine's run directory, where the VMM creates its API
	// socket and its vsock socket and mvmd connects to them by path (ADR 0015).
	// The Supervision port names it and the supervisor is given the same.
	RunDir string
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

	// Tap, Guest and RunDir are the lifecycle's, never the Driver's: For overwrites
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
	// RunDir is the Machine's run directory, the one its BootSpec names. The
	// supervisor bind-mounts it read-write in the VMM's root.
	RunDir string
}

// For returns the description with the parts of it that come from the
// Machine's boot rather than from the Driver (ADR 0015).
func (p ProcessDescription) For(boot BootSpec) ProcessDescription {
	p.Tap, p.Guest, p.RunDir = boot.Network.Tap, boot.Resources, boot.RunDir
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
	// Boot configures the started VMM and boots the guest, from the boot
	// description Describe was given: the lifecycle holds it and passes it again,
	// so a Driver keeps nothing between the two, and reads nothing back from a
	// directory the VMM's user can write.
	Boot(ctx context.Context, id MachineID, spec BootSpec) error
	// GuestConn opens a new byte stream to the guest's port of the Machine and
	// returns it, ready for the Guest Agent's protocol: whatever the VMM needs
	// before the first byte reaches the guest (a connect request to its vsock
	// socket in the run directory, for one) is the Driver's, and is finished
	// before this returns. The caller closes the stream and opens another
	// after a break. A Machine whose VMM is not running, or a guest that
	// refuses the port, is ErrGuestUnreachable.
	GuestConn(ctx context.Context, id MachineID, port uint32) (net.Conn, error)
	// Console opens a new session to the console of the Machine and returns it:
	// what the guest writes to its console is read from it, and what is written
	// to it reaches the guest's console as input. Each call opens its own
	// session, and sessions do not affect one another or the Machine. The
	// caller closes the session; a session also ends when the Machine's VMM
	// does, reading to io.EOF. A Machine whose VMM is not running is
	// ErrConsoleUnavailable.
	Console(ctx context.Context, id MachineID) (io.ReadWriteCloser, error)
	// ConsoleLog returns what the Machine's console has written, from the
	// start of its run, up to the moment of the call. After the Machine's VMM
	// has ended, it is all the console ever wrote: establishing that nothing is
	// still on its way to the log is the Driver's. A Machine with no log is
	// ErrConsoleUnavailable.
	ConsoleLog(ctx context.Context, id MachineID) (io.ReadCloser, error)
	// Shutdown asks the guest to stop and waits up to grace for it to do so.
	Shutdown(ctx context.Context, id MachineID, grace time.Duration) error
	Pause(ctx context.Context, id MachineID) error
	Resume(ctx context.Context, id MachineID) error
	// AttachDisk and DetachDisk need CapabilityDiskHotplug.
	AttachDisk(ctx context.Context, id MachineID, dev BlockDevice) error
	DetachDisk(ctx context.Context, id MachineID, disk DiskID) error
	// Resize needs CapabilityResize.
	Resize(ctx context.Context, id MachineID, target Resources) error
	// Snapshot, DescribeRestore and Restore need CapabilitySnapshot.
	//
	// Snapshot writes the paused Machine's memory and device state to files
	// in its run directory, under the same names on every Snapshot of a
	// Machine, so the files a failed attempt left are written over by the next.
	Snapshot(ctx context.Context, id MachineID) (DriverSnapshot, error)
	// DescribeRestore is Describe for a Machine that boots from the files of a
	// snapshot. The boot's network device is the new Machine's tap, not the one
	// the snapshot recorded.
	DescribeRestore(ctx context.Context, id MachineID, spec BootSpec, files []string) (ProcessDescription, error)
	// Restore loads the snapshot into the started VMM and resumes the guest.
	Restore(ctx context.Context, id MachineID) error
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
