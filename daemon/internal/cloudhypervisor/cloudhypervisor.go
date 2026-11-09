// Package cloudhypervisor is the VMM Driver for Cloud Hypervisor v54.0
// (driver.md). It describes the process the supervisor starts, which carries
// the whole VM configuration on its command line, and drives the started VMM
// through its API socket in the Machine's run directory.
package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Version is the one Cloud Hypervisor release this Driver is written against.
// Boot refuses a VMM that reports another.
const Version = "54.0"

const (
	apiSocketName   = "api.sock"
	vsockSocketName = "vsock.sock"
	// logName is Cloud Hypervisor's own log in the run directory, which keeps
	// the supervisor's capture of its output (vmm.log) to the guest's console.
	logName = "cloud-hypervisor.log"
	// guestCID is the guest's vsock address; it is the same for every Machine
	// since a vsock device is private to its VMM.
	guestCID = 3
)

// RunDirs names a Machine's run directory, where the API and vsock sockets are.
type RunDirs interface {
	RunDir(id machine.MachineID) (string, error)
}

// Config is what the node says about Cloud Hypervisor and the guest it boots
// (ADR 0010, ADR 0015). The digests are SHA-256, in hex.
type Config struct {
	Binary       string
	BinarySHA256 string
	Kernel       string
	KernelSHA256 string
	Initrd       string
	Dirs         RunDirs
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate refuses a configuration that names no absolute path or no SHA-256
// digest where one is due. It reads no file.
func (c Config) Validate() error {
	for _, f := range []struct{ name, path string }{{"binary", c.Binary}, {"kernel", c.Kernel}, {"initrd", c.Initrd}} {
		if !filepath.IsAbs(f.path) {
			return fmt.Errorf("cloud-hypervisor %s %q is not an absolute path", f.name, f.path)
		}
	}
	for _, f := range []struct{ name, sum string }{{"binary", c.BinarySHA256}, {"kernel", c.KernelSHA256}} {
		if !sha256Hex.MatchString(f.sum) {
			return fmt.Errorf("cloud-hypervisor %s digest %q is not 64 lowercase hex digits", f.name, f.sum)
		}
	}
	return nil
}

// Driver implements machine.Driver for Cloud Hypervisor.
type Driver struct {
	cfg      Config
	cpuModel string
}

// New returns the Driver. It refuses a binary or a kernel whose digest is not
// the configured one, so a node fails at start and not at its first boot.
func New(cfg Config) (*Driver, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.Dirs == nil {
		return nil, fmt.Errorf("cloud-hypervisor needs the run directories of Machines")
	}
	if err := verify(cfg); err != nil {
		return nil, err
	}
	model, err := hostCPUModel()
	if err != nil {
		return nil, err
	}
	return &Driver{cfg: cfg, cpuModel: model}, nil
}

// Info implements machine.Driver. The one optional capability reported is the
// console. Snapshot, disk hot-plug and resize are in v54.0 but not yet in this
// Driver, and the operations that need them refuse (unsupported).
func (d *Driver) Info() machine.DriverInfo {
	return machine.DriverInfo{Name: "cloud-hypervisor", Version: Version, CPUModel: d.cpuModel,
		Capabilities: []machine.Capability{machine.CapabilityConsole}}
}

// Observe implements machine.Driver. The Driver raises no event of its own:
// the supervisor's pidfds tell the lifecycle when a VMM has ended.
func (d *Driver) Observe(func(machine.DriverEvent)) {}

// inRun is the path of a file of the run directory in the VMM's root, which is
// what the VMM is told; mvmd itself reaches the file at the run directory's
// host path.
func inRun(name string) string { return filepath.Join(machine.InRunDir, name) }

func (d *Driver) socket(id machine.MachineID, name string) (string, error) {
	dir, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

func (d *Driver) api(id machine.MachineID) (*client, error) {
	sock, err := d.socket(id, apiSocketName)
	if err != nil {
		return nil, err
	}
	return newClient(sock), nil
}

// unsupported is the refusal of an operation this Driver does not implement:
// the capability it needs is not reported.
func unsupported(what string) error {
	return fmt.Errorf("%w: the Cloud Hypervisor Driver does not implement %s", machine.ErrUnsupported, what)
}

// AttachDisk implements machine.Driver and always refuses.
func (d *Driver) AttachDisk(context.Context, machine.MachineID, machine.BlockDevice) error {
	return unsupported("disk hot-plug")
}

// DetachDisk implements machine.Driver and always refuses.
func (d *Driver) DetachDisk(context.Context, machine.MachineID, machine.DiskID) error {
	return unsupported("disk hot-plug")
}

// Drive implements machine.Driver and always refuses.
func (d *Driver) Drive(context.Context, machine.MachineID, machine.DiskID) (machine.DriveState, error) {
	return machine.DriveAbsent, unsupported("disk hot-plug")
}

// Resize implements machine.Driver and always refuses.
func (d *Driver) Resize(context.Context, machine.MachineID, machine.Resources) error {
	return unsupported("resize")
}

// Snapshot implements machine.Driver and always refuses.
func (d *Driver) Snapshot(context.Context, machine.MachineID) (machine.DriverSnapshot, error) {
	return machine.DriverSnapshot{}, unsupported("snapshot")
}

// DescribeRestore implements machine.Driver and always refuses.
func (d *Driver) DescribeRestore(context.Context, machine.MachineID, machine.BootSpec, []string) (machine.ProcessDescription, error) {
	return machine.ProcessDescription{}, unsupported("restore")
}

// Restore implements machine.Driver and always refuses.
func (d *Driver) Restore(context.Context, machine.MachineID, machine.BootSpec) error {
	return unsupported("restore")
}
