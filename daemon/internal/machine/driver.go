package machine

import (
	"context"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

// ErrConsoleUnavailable is a console, or the log of one, that a Driver cannot
// open.
var ErrConsoleUnavailable = errors.New("the console is not available")

// ErrVMMEnded is a VMM that has ended, so the guest port it would have
// answered on no longer exists.
var ErrVMMEnded = errors.New("the VMM has ended")

// GuestAgentPort is the port the Guest Agent listens on in every guest
// (guest.md), the one a caller of Driver.GuestConn names for it.
const GuestAgentPort uint32 = 1024

// Capability is an optional operation a Driver may support.
type Capability string

const (
	// CapabilityDiskHotplug attaches and detaches disks of a Running or
	// Paused Machine.
	CapabilityDiskHotplug Capability = "disk-hotplug"
	// CapabilityCPUResize changes the vCPUs of a Running or Paused Machine.
	CapabilityCPUResize Capability = "cpu-resize"
	// CapabilityMemoryResize changes the memory of a Running or Paused
	// Machine.
	CapabilityMemoryResize Capability = "memory-resize"
	// CapabilityConsole opens sessions to the console of a Running or Paused
	// Machine. The console log is not part of it.
	CapabilityConsole Capability = "console"
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
	// Limits is the most of a Machine the Driver accepts. The Driver enforces
	// the same figures in Describe.
	Limits DriverLimits
}

// DriverLimits are the upper bounds a Driver enforces on a Machine's
// resources and ceiling. A zero figure states no bound.
type DriverLimits struct {
	MaxVCPUs uint32
}

// DriverSnapshot is what a Driver made of a paused Machine: the files it
// wrote in the run directory. It is the Driver's own and the inside does not
// read the files.
type DriverSnapshot struct {
	Files []string
}

// BlockDevice is a raw disk file handed to a VMM.
type BlockDevice struct {
	Disk     DiskID
	Path     string
	ReadOnly bool
}

// DriveState is whether a running VMM has a drive for a disk, and how it came
// by it.
type DriveState int

const (
	// DriveAbsent is a disk the VMM has no drive for.
	DriveAbsent DriveState = iota
	// DriveAtBoot is a drive the VMM was started with, so the guest has it
	// from its own boot and was never asked to find it.
	DriveAtBoot
	// DriveHotPlugged is a drive added to the running VMM, which the guest
	// finds when it is asked (ADR 0022).
	DriveHotPlugged
)

// NetworkDevice is a tap's name and the guest MAC; the Driver learns no
// address and no policy.
type NetworkDevice struct {
	Tap string
	MAC string
}

// BootSpec is what a Driver needs from the lifecycle to boot a Machine.
// Devices are in boot order: the image, then the root disk.
type BootSpec struct {
	Devices []BlockDevice
	// RecordedDisks are the disks a restored snapshot recorded, which its saved
	// state already holds (ADR 0041). A Driver does not bind them as devices:
	// it lists each as a run file for the supervisor to link (ADR 0042). A boot
	// has none; it hot-plugs its disks.
	RecordedDisks []BlockDevice
	Network       NetworkDevice
	// Kernel is the guest kernel of a boot; a restore carries none, since a
	// restored guest is already running one (ADR 0045).
	Kernel    KernelFile
	Resources Resources
	// Ceiling is the most the guest can have (ADR 0029). A figure left zero is
	// the figure of Resources.
	Ceiling Resources
	// RunDir is the Machine's run directory on the host, where mvmd connects to
	// the VMM's API socket and vsock socket by path (ADR 0015); the VMM names
	// them under InRunDir. The Supervision port names it and the supervisor is
	// given the same.
	RunDir string
}

// The paths a VMM sees its Machine's files at, inside its root (ADR 0019).
// They are the same for every Machine, so nothing a VMM records of a Machine
// names a host path, and a Machine restored from that record has its own files
// bound to the same paths. A Driver describes devices to its VMM by these paths
// only.
const (
	// InKernel and InInitrd are the guest kernel and the initrd.
	InKernel = "/boot/kernel"
	InInitrd = "/boot/initrd"
	// InRunDir is the Machine's run directory, where the VMM creates its API
	// socket and its vsock socket.
	InRunDir = "/run"
	// InSnapshotDir is the directory of a restored VMM's root where the files
	// of the snapshot are bound, read-only. A Driver names its own files under
	// it, as it names its sockets under InRunDir.
	InSnapshotDir = "/snapshot"
)

// InDisk is the path of the block device at a position of BootSpec.Devices.
// For a Machine the positions are its image (0), its root disk (1) and its
// attached disks in the order they were attached; a converter has its output
// disk at 0. A Driver learns no more of what a device holds (ADR 0010).
func InDisk(position int) string { return "/disk/" + strconv.Itoa(position) }

// File is a host file made visible to the VMM at a path in its root.
type File struct {
	// Host is the file's path on the host, which the supervisor opens.
	Host string
	// In is the file's path in the VMM's root: one of the names above.
	In       string
	ReadOnly bool
	// SHA256 is the file's digest in hex, or empty for a file that is not
	// pinned. The supervisor checks it against the file it opens and mounts
	// that file, so the file checked is the file the VMM is given (ADR 0019).
	SHA256 string
}

// DeviceFiles are the devices of a boot description as files at their fixed
// paths, in boot order.
func DeviceFiles(devices []BlockDevice) []File {
	files := make([]File, 0, len(devices))
	for i, dev := range devices {
		files = append(files, File{Host: dev.Path, In: InDisk(i), ReadOnly: dev.ReadOnly})
	}
	return files
}

// PipeTrigger is a pipe the supervisor creates and passes the write end of to
// the VMM as descriptor FD, and the bytes it ends the VMM on when it finds them
// in what the VMM writes. The zero value, with no Match, is no pipe.
type PipeTrigger struct {
	FD    int
	Match []byte
}

// ProcessDescription is the VMM process a Driver describes for the
// supervisor to start and confine. It grants no capability, chooses no user
// and skips no step (ADR 0015).
type ProcessDescription struct {
	// Executable is a host path, and is at the same path in the VMM's root.
	Executable string
	// ExecutableSHA256 is its digest in hex, or empty when it is not pinned;
	// it is checked as File.SHA256 is.
	ExecutableSHA256 string
	Args             []string
	// BootFiles are the files of the boot description, with their modes: at
	// the fixed paths above.
	BootFiles []File
	// ReadFiles are host files the VMM reads beyond the boot files.
	ReadFiles []string
	// RunFiles are host files the supervisor hard links into the run directory,
	// each under its Name, after it creates the directory and before it starts
	// the VMM (ADR 0042). A name or path it refuses, or a link it cannot make,
	// fails the start.
	RunFiles []RunFile
	// DeviceNodes are the device nodes the VMM opens.
	DeviceNodes []string
	// TapByDescriptor is true when the VMM takes its tap by descriptor.
	TapByDescriptor bool
	// EndOn is a pipe the supervisor ends the VMM on (ADR 0026), for a VMM that
	// outlives a guest reset. The zero value is no pipe.
	EndOn    PipeTrigger
	Overhead ProcessOverhead

	// Tap, Guest and RunDir are the lifecycle's, never the Driver's: For overwrites
	// them from the Machine, so a Driver cannot misstate what the cgroup's
	// limits are computed from.
	//
	// Tap is the Machine's tap, empty for a VMM with no network device. The
	// supervisor opens it and passes it as descriptor 3 when TapByDescriptor is
	// set; a VMM that opens it by name shares the host's network namespace, and
	// every other VMM gets one of its own.
	Tap string
	// Guest is the most memory and vCPUs the guest can have, its ceiling
	// (ADR 0029), which with Overhead fix the VMM's cgroup limits.
	Guest Resources
	// RunDir is the Machine's run directory on the host, the one its BootSpec
	// names. The supervisor bind-mounts it read-write at InRunDir in the VMM's
	// root.
	RunDir string
}

// RunFile is a host file the supervisor hard links into the Machine's run
// directory before it starts the VMM (ADR 0042).
type RunFile struct {
	// Name is the link's name in the run directory: one file name.
	Name string
	// Path is the host file's absolute path.
	Path string
}

// For returns the description with the parts of it that come from the
// Machine's boot rather than from the Driver (ADR 0015).
func (p ProcessDescription) For(boot BootSpec) ProcessDescription {
	p.Tap, p.Guest, p.RunDir = boot.Network.Tap, boot.Ceiling.OrElse(boot.Resources), boot.RunDir
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
//
// An operation that needs a Capability the Driver does not report returns an
// error wrapping ErrUnsupported. The operations are Console (CapabilityConsole),
// AttachDisk, DetachDisk and Drive (CapabilityDiskHotplug), Resize
// (CapabilityCPUResize for the vCPUs, CapabilityMemoryResize for the memory),
// and Snapshot, DescribeRestore and Restore
// (CapabilitySnapshot).
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
	// after a break. A Machine whose VMM has ended is ErrVMMEnded joined
	// with ErrGuestUnreachable; a guest that refuses the port is
	// ErrGuestUnreachable.
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
	// AttachDisk, DetachDisk and Drive need CapabilityDiskHotplug.
	AttachDisk(ctx context.Context, id MachineID, dev BlockDevice) error
	DetachDisk(ctx context.Context, id MachineID, disk DiskID) error
	// Drive reports whether the running VMM of the Machine has a drive for the
	// disk and how it came by it (ADR 0023). It reads the VMM and changes
	// nothing.
	Drive(ctx context.Context, id MachineID, disk DiskID) (DriveState, error)
	// Resize needs the capability of each resource the target changes.
	Resize(ctx context.Context, id MachineID, target Resources) error
	// Snapshot, DescribeRestore and Restore need CapabilitySnapshot.
	//
	// Snapshot writes the paused Machine's memory and device state to files
	// in its run directory, under the same names on every Snapshot of a
	// Machine, so the files a failed attempt left are written over by the next.
	Snapshot(ctx context.Context, id MachineID) (DriverSnapshot, error)
	// DescribeRestore is Describe for a Machine that boots from the files of a
	// snapshot. The boot's network device is the new Machine's tap, not the one
	// the snapshot recorded. Its boot files are exactly those its VMM opens on
	// restore, at their fixed paths and none other (ADR 0027).
	DescribeRestore(ctx context.Context, id MachineID, spec BootSpec, files []string) (ProcessDescription, error)
	// Restore sees the snapshot loaded into the started VMM, which a VMM that
	// restores from its command line has done as it started, and the guest
	// resumed. It is given the boot description DescribeRestore was, as Boot
	// is, so the Driver keeps nothing between the two calls.
	Restore(ctx context.Context, id MachineID, spec BootSpec) error
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
