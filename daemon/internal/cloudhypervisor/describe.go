package cloudhypervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	mib = 1 << 20
	// maxVCPUs bounds a Machine's vCPUs; v54.0 accepts more, and the limit is
	// the one the Machines of every Driver share.
	maxVCPUs = 32
	// overheadMemory is added to the guest's memory for the VMM's cgroup. It is
	// the figure the Firecracker Driver measured, not one measured for Cloud
	// Hypervisor (ADR 0015: the peak, not the average).
	overheadMemory = 128 * mib
	// overheadTasks is an estimate of the VMM's tasks beside its vCPUs, not
	// measured for Cloud Hypervisor.
	overheadTasks = 16
	// overheadCPUs is the CPU the main thread takes for device emulation.
	overheadCPUs = 1.0
	// tapDescriptor is the descriptor the supervisor passes the tap as.
	tapDescriptor = 3
)

// cmdline is the kernel command line: the serial console the VMM carries to
// the run directory's log. x86_64's serial port is ttyS0 and aarch64's PL011 is
// ttyAMA0 (docs/device_model.md).
func cmdline(goarch string) string {
	if goarch == "arm64" {
		return "console=ttyAMA0"
	}
	return "console=ttyS0"
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
			return fmt.Errorf("check the cloud-hypervisor %s: %w", f.name, err)
		}
		if got != f.want {
			return fmt.Errorf("the cloud-hypervisor %s %s has digest %s, not the configured %s", f.name, f.path, got, f.want)
		}
	}
	if _, err := os.Stat(filepath.Clean(cfg.Initrd)); err != nil {
		return fmt.Errorf("check the cloud-hypervisor initrd: %w", err)
	}
	return nil
}

// Describe implements machine.Driver. It states the process, whose arguments
// are the VM's whole configuration, and what it needs open, with the digests of
// the pinned binary and kernel for the supervisor to check; it reads neither
// file. It keeps nothing: Boot is given the boot description again.
func (d *Driver) Describe(_ context.Context, _ machine.MachineID, spec machine.BootSpec) (machine.ProcessDescription, error) {
	if err := validate(spec); err != nil {
		return machine.ProcessDescription{}, err
	}
	boot := []machine.File{
		{Host: d.cfg.Kernel, In: machine.InKernel, ReadOnly: true, SHA256: d.cfg.KernelSHA256},
		{Host: d.cfg.Initrd, In: machine.InInitrd, ReadOnly: true},
	}
	return machine.ProcessDescription{
		Executable:       d.cfg.Binary,
		ExecutableSHA256: d.cfg.BinarySHA256,
		Args:             args(spec, runtime.GOARCH),
		BootFiles:        append(boot, machine.DeviceFiles(spec.Devices)...),
		ReadFiles:        cpuReadFiles(runtime.GOARCH),
		// /dev/urandom is the source of the virtio-rng device Cloud Hypervisor
		// adds by default; without it the VM does not boot.
		DeviceNodes:     []string{"/dev/kvm", "/dev/urandom"},
		TapByDescriptor: spec.Network.Tap != "",
		Overhead: machine.ProcessOverhead{
			MemoryBytes: overheadMemory,
			Tasks:       spec.Resources.VCPUs + overheadTasks + uint32(len(spec.Devices)),
			CPUs:        overheadCPUs,
		},
	}, nil
}

// args is the command line that configures the VM. Paths are those in the VMM's
// root, and the guest's serial console is the VMM's standard output and input
// with no virtio console beside it. The network device is the tap the
// supervisor passes as a descriptor: the API takes it by name only.
func args(spec machine.BootSpec, goarch string) []string {
	a := []string{
		"--api-socket", "path=" + inRun(apiSocketName),
		"--log-file", inRun(logName),
		"--kernel", machine.InKernel,
		"--initramfs", machine.InInitrd,
		"--cmdline", cmdline(goarch),
		"--cpus", "boot=" + strconv.FormatUint(uint64(spec.Resources.VCPUs), 10),
		"--memory", "size=" + strconv.FormatUint(spec.Resources.MemoryBytes/mib, 10) + "M",
		"--serial", "tty",
		"--console", "off",
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", guestCID, inRun(vsockSocketName)),
		"--disk",
	}
	for i, dev := range spec.Devices {
		ro := "off"
		if dev.ReadOnly {
			ro = "on"
		}
		a = append(a, fmt.Sprintf("path=%s,readonly=%s,image_type=raw,id=disk%d", machine.InDisk(i), ro, i))
	}
	if spec.Network.Tap != "" {
		a = append(a, "--net", fmt.Sprintf("fd=[%d],mac=%s", tapDescriptor, spec.Network.MAC))
	}
	return a
}

// cpuReadFiles are the host files the VMM reads beyond the boot files: on
// aarch64 the CPU cache description (ADR 0015).
func cpuReadFiles(goarch string) []string {
	if goarch != "arm64" {
		return nil
	}
	return []string{"/sys/devices/system/cpu/cpu0/cache"}
}

// maxTapName is the longest interface name Linux holds: IFNAMSIZ less the NUL.
const maxTapName = 15

// validate refuses what v54.0 or the kernel would, before a process exists for
// it.
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
