// Package firecracker is the VMM Driver for Firecracker v1.17.0 (driver.md).
// It describes the process the supervisor starts and drives the started VMM
// through its API socket in the Machine's run directory.
package firecracker

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"runtime"

	"github.com/garamsh/mvmd/daemon/internal/hostcpu"
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
		return fmt.Errorf("firecracker lists no kernel")
	}
	for _, f := range files {
		if !filepath.IsAbs(f.path) {
			return fmt.Errorf("firecracker %s %q is not an absolute path", f.name, f.path)
		}
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
	// goarch is the architecture the VMM runs on; whether Shutdown can send Ctrl-Alt-Del depends on it.
	goarch string
}

// New returns the Driver. It refuses a binary, a kernel or an initrd whose digest is not
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
	model, err := hostcpu.Model()
	if err != nil {
		return nil, err
	}
	return &Driver{cfg: cfg, cpuModel: model, goarch: runtime.GOARCH}, nil
}

// Info implements machine.Driver. The optional capabilities reported are the
// console, snapshot, which covers restore, and disk hot-plug. Resize is not
// one: v1.17.0 has no resize this Driver can reach (Resize says why).
func (d *Driver) Info() machine.DriverInfo {
	return machine.DriverInfo{Name: "firecracker", Version: Version, CPUModel: d.cpuModel,
		Limits:       machine.DriverLimits{MaxVCPUs: maxVCPUs},
		Capabilities: []machine.Capability{machine.CapabilityConsole, machine.CapabilitySnapshot, machine.CapabilityDiskHotplug}}
}

// Observe implements machine.Driver. It raises no event; see docs/architecture/driver.md.
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

// Resize implements machine.Driver and always refuses: Firecracker v1.17.0
// takes the vCPU count before boot only, and changes memory only through a
// virtio-mem region or a balloon that is configured before boot, neither of
// which Boot configures. The Driver does not report the capability, so the
// lifecycle refuses a resize of a hot Machine before it gets here.
func (d *Driver) Resize(context.Context, machine.MachineID, machine.Resources) error {
	return fmt.Errorf("%w: Firecracker v1.17.0 sets the vCPU count before boot only "+
		"(PUT and PATCH /machine-config are \"Pre-boot only\" in firecracker.yaml), and resizes memory "+
		"only through a virtio-mem region (docs/memory-hotplug.md) or a balloon (PUT /balloon, \"before machine startup\"), "+
		"which a Machine's boot does not configure", machine.ErrUnsupported)
}
