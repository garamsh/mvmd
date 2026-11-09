// Package firecracker is the VMM Driver for Firecracker v1.17.0 (driver.md).
// It describes the process the supervisor starts and drives the started VMM
// through its API socket in the Machine's run directory.
package firecracker

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Version is the one Firecracker release this Driver is written against. Boot
// refuses a VMM that reports another.
const Version = "1.17.0"

const (
	apiSocketName   = "api.sock"
	vsockSocketName = "vsock.sock"
	// logName is Firecracker's own log in the run directory, which keeps the
	// supervisor's capture of its output (vmm.log) to the guest's console.
	logName = "firecracker.log"
	// guestCID is the guest's vsock address; it is the same for every Machine
	// since a vsock device is private to its VMM.
	guestCID = 3
)

// RunDirs names a Machine's run directory, where the API and vsock sockets are.
type RunDirs interface {
	RunDir(id machine.MachineID) (string, error)
}

// Config is what the node says about Firecracker and the guest it boots
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
			return fmt.Errorf("firecracker %s %q is not an absolute path", f.name, f.path)
		}
	}
	for _, f := range []struct{ name, sum string }{{"binary", c.BinarySHA256}, {"kernel", c.KernelSHA256}} {
		if !sha256Hex.MatchString(f.sum) {
			return fmt.Errorf("firecracker %s digest %q is not 64 lowercase hex digits", f.name, f.sum)
		}
	}
	return nil
}

// Driver implements machine.Driver for Firecracker.
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
		return nil, fmt.Errorf("firecracker needs the run directories of Machines")
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

// Info implements machine.Driver. The optional capabilities reported are the
// console and snapshot, which covers restore: this Driver does not yet
// hot-plug or resize.
func (d *Driver) Info() machine.DriverInfo {
	return machine.DriverInfo{Name: "firecracker", Version: Version, CPUModel: d.cpuModel,
		Capabilities: []machine.Capability{machine.CapabilityConsole, machine.CapabilitySnapshot}}
}

// Observe implements machine.Driver. The Driver raises no event of its own:
// the supervisor's pidfds tell the lifecycle when a VMM has ended.
func (d *Driver) Observe(func(machine.DriverEvent)) {}

// inRun is the path of a file of the run directory in the VMM's root, which is
// what the VMM is told; mvmd itself reaches the file at the run directory's
// host path.
func inRun(name string) string { return filepath.Join(machine.InRunDir, name) }

// inSnapshot is the path of a snapshot file in a restored VMM's root.
func inSnapshot(name string) string { return filepath.Join(machine.InSnapshotDir, name) }

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

func unsupported(op string) error {
	return fmt.Errorf("%w: the Firecracker Driver does not yet %s", machine.ErrUnsupported, op)
}

// AttachDisk implements machine.Driver.
func (d *Driver) AttachDisk(context.Context, machine.MachineID, machine.BlockDevice) error {
	return unsupported("attach a disk to a running Machine")
}

// DetachDisk implements machine.Driver.
func (d *Driver) DetachDisk(context.Context, machine.MachineID, machine.DiskID) error {
	return unsupported("detach a disk from a running Machine")
}

// Resize implements machine.Driver. Firecracker v1.17.0 takes a vCPU count
// only before boot.
func (d *Driver) Resize(context.Context, machine.MachineID, machine.Resources) error {
	return unsupported("resize a running Machine")
}
