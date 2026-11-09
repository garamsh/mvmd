package firecracker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	mib = 1 << 20
	// maxSocketPath is the longest path a Unix socket address holds: sun_path
	// is 108 bytes with its terminating NUL.
	maxSocketPath = 107
	// maxVCPUs is the most vCPUs v1.17.0 accepts (swagger MachineConfiguration).
	maxVCPUs = 32
	// overheadMemory is added to the guest's memory for the VMM's cgroup. An
	// idle v1.17.0 with a 256 MiB guest and 2 vCPUs held 60 MiB resident, the
	// guest's touched pages among them; the rest covers the page tables and
	// device state a busy guest adds (ADR 0015: the peak, not the average).
	overheadMemory = 128 * mib
	// overheadTasks is the VMM's tasks beside its vCPUs: its main and API
	// threads, the kernel's NX huge page recovery thread KVM starts for it, and
	// a margin. Devices run on the main thread with the Sync disk engine.
	overheadTasks = 8
	// overheadCPUs is the CPU the main thread takes for device emulation.
	overheadCPUs = 1.0
)

// bootArgs is the kernel command line: the serial console the VMM carries to
// the run directory's log, and a reset that ends the VMM. i8042's probe
// parameters are the ones the Firecracker documentation gives to skip the
// probe, and apply on x86_64 only.
func bootArgs(goarch string) string {
	args := "console=ttyS0 reboot=k panic=1"
	if goarch == "amd64" {
		args += " i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
	}
	return args
}

// digest returns the SHA-256 of the file, in hex.
func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// verify refuses a binary or a kernel that is not the one the node pinned. It
// is how a node fails at its start and not at its first boot; the check of
// record is the supervisor's, on the file it presents to the VMM (ADR 0019).
func verify(cfg Config) error {
	for _, f := range []struct{ name, path, want string }{
		{"binary", cfg.Binary, cfg.BinarySHA256},
		{"kernel", cfg.Kernel, cfg.KernelSHA256},
	} {
		got, err := digest(f.path)
		if err != nil {
			return fmt.Errorf("check the firecracker %s: %w", f.name, err)
		}
		if got != f.want {
			return fmt.Errorf("the firecracker %s %s has digest %s, not the configured %s", f.name, f.path, got, f.want)
		}
	}
	if _, err := os.Stat(filepath.Clean(cfg.Initrd)); err != nil {
		return fmt.Errorf("check the firecracker initrd: %w", err)
	}
	return nil
}

// Describe implements machine.Driver. It states the process and what it needs
// open, with the digests of the pinned binary and kernel for the supervisor to
// check; it reads neither file. It keeps nothing: Boot is given the boot
// description again.
func (d *Driver) Describe(_ context.Context, _ machine.MachineID, spec machine.BootSpec) (machine.ProcessDescription, error) {
	boot := []machine.File{
		{Host: d.cfg.Kernel, In: machine.InKernel, ReadOnly: true, SHA256: d.cfg.KernelSHA256},
		{Host: d.cfg.Initrd, In: machine.InInitrd, ReadOnly: true},
	}
	return d.process(spec, append(boot, machine.DeviceFiles(spec.Devices)...))
}

// process states the VMM process for a boot description, given the files the
// VMM is to see: those of a boot, or those of a restore.
func (d *Driver) process(spec machine.BootSpec, files []machine.File) (machine.ProcessDescription, error) {
	if err := validate(spec); err != nil {
		return machine.ProcessDescription{}, err
	}
	// mvmd connects to the sockets at their host paths and the VMM creates them
	// at their paths in its root; both are under the 107 bytes a Unix socket
	// address holds.
	for _, p := range []string{filepath.Join(spec.RunDir, apiSocketName), filepath.Join(spec.RunDir, vsockSocketName)} {
		if len(p) > maxSocketPath {
			return machine.ProcessDescription{}, fmt.Errorf("the socket path %q is %d bytes, over the %d a Unix socket holds", p, len(p), maxSocketPath)
		}
	}

	nodes := []string{"/dev/kvm"}
	if spec.Network.Tap != "" {
		nodes = append(nodes, "/dev/net/tun")
	}
	return machine.ProcessDescription{
		Executable:       d.cfg.Binary,
		ExecutableSHA256: d.cfg.BinarySHA256,
		Args:             []string{"--api-sock", inRun(apiSocketName), "--enable-pci", "--log-path", inRun(logName)},
		BootFiles:        files,
		ReadFiles:        cpuReadFiles(runtime.GOARCH),
		DeviceNodes:      nodes,
		Overhead: machine.ProcessOverhead{
			MemoryBytes: overheadMemory,
			Tasks:       spec.Resources.VCPUs + overheadTasks,
			CPUs:        overheadCPUs,
		},
	}, nil
}

// cpuReadFiles are the host files the VMM reads beyond the boot files: on
// aarch64 the CPU cache description and the MIDR register (ADR 0015).
func cpuReadFiles(goarch string) []string {
	if goarch != "arm64" {
		return nil
	}
	return []string{"/sys/devices/system/cpu/cpu0/cache", "/sys/devices/system/cpu/cpu0/regs/identification/midr_el1"}
}

// maxTapName is the longest interface name Linux holds: IFNAMSIZ less the NUL.
const maxTapName = 15

// validate refuses what v1.17.0 or the kernel would, before a process exists
// for it.
func validate(spec machine.BootSpec) error {
	switch {
	case len(spec.Network.Tap) > maxTapName:
		return fmt.Errorf("the tap name %q is %d bytes, over the %d an interface name holds", spec.Network.Tap, len(spec.Network.Tap), maxTapName)
	case !filepath.IsAbs(spec.RunDir):
		return fmt.Errorf("the run directory %q is not an absolute path", spec.RunDir)
	case spec.Resources.VCPUs < 1 || spec.Resources.VCPUs > maxVCPUs:
		return fmt.Errorf("%d vCPUs is outside 1 to %d", spec.Resources.VCPUs, maxVCPUs)
	case spec.Resources.MemoryBytes == 0 || spec.Resources.MemoryBytes%mib != 0:
		return fmt.Errorf("%d bytes of memory is not a whole number of MiB", spec.Resources.MemoryBytes)
	case len(spec.Devices) == 0:
		return fmt.Errorf("a Machine boots with at least one block device")
	}
	for _, dev := range spec.Devices {
		if !filepath.IsAbs(dev.Path) {
			return fmt.Errorf("the block device path %q is not absolute", dev.Path)
		}
	}
	return nil
}
