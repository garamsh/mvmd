// Package cloudhypervisor is the VMM Driver for Cloud Hypervisor v54.0
// (driver.md). It describes the process the supervisor starts, which carries
// the whole VM configuration on its command line, and drives the started VMM
// through its API socket in the Machine's run directory.
package cloudhypervisor

import (
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/garamsh/mvmd/daemon/internal/hostcpu"
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
	// Kernels are the guest kernels the node offers; a boot is given the one
	// it uses in BootSpec.Kernel (ADR 0045). Each is verified at New.
	Kernels      []machine.KernelFile
	Initrd       string
	InitrdSHA256 string
	Dirs         RunDirs
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate refuses a configuration that names no absolute path or no SHA-256
// digest where one is due. It reads no file.
func (c Config) Validate() error {
	files := []struct{ name, path, sum string }{{"binary", c.Binary, c.BinarySHA256}, {"initrd", c.Initrd, c.InitrdSHA256}}
	for _, k := range c.Kernels {
		files = append(files, struct{ name, path, sum string }{"kernel", k.Path, k.SHA256})
	}
	if len(c.Kernels) == 0 {
		return fmt.Errorf("cloud-hypervisor lists no kernel")
	}
	for _, f := range files {
		if !filepath.IsAbs(f.path) {
			return fmt.Errorf("cloud-hypervisor %s %q is not an absolute path", f.name, f.path)
		}
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
	// goarch is the architecture the VMM runs on; what a resize needs depends on it.
	goarch string
}

// New returns the Driver. It refuses a binary, a kernel or an initrd whose digest is not
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
	model, err := hostcpu.Model()
	if err != nil {
		return nil, err
	}
	return &Driver{cfg: cfg, cpuModel: model, goarch: runtime.GOARCH}, nil
}

// Info implements machine.Driver. The optional capabilities reported are the
// console, snapshot and disk hot-plug, and on x86_64 resize: v54.0's vCPU and
// memory hot-plug are x86_64's, and elsewhere Resize refuses (unsupported).
func (d *Driver) Info() machine.DriverInfo {
	caps := []machine.Capability{machine.CapabilityConsole, machine.CapabilitySnapshot, machine.CapabilityDiskHotplug}
	if resizeCapable(d.goarch) {
		caps = append(caps, machine.CapabilityCPUResize, machine.CapabilityMemoryResize)
	}
	return machine.DriverInfo{Name: "cloud-hypervisor", Version: Version, CPUModel: d.cpuModel, Capabilities: caps,
		Limits: machine.DriverLimits{MaxVCPUs: maxVCPUs}}
}

// Observe implements machine.Driver. It raises no event; see docs/architecture/driver.md.
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
