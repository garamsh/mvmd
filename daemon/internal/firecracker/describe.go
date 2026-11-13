package firecracker

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
)

const (
	mib = 1 << 20
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
// the run directory's log, a reset that ends the VMM, and the method the guest
// ends its Machine by (ADR 0032). i8042's probe parameters are the ones the
// Firecracker documentation gives to skip the probe, and apply on x86_64 only.
// On x86_64 the VMM ends on the keyboard-controller reset, and a power off
// halts the guest with the VMM running. On aarch64 PSCI turns both into a KVM
// system event the VMM ends on, so the plain power off is used.
func bootArgs(goarch string) string {
	args := "console=ttyS0 reboot=k panic=1"
	if goarch == "amd64" {
		return args + " i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd mvmd.end=reset"
	}
	return args + " mvmd.end=poweroff"
}

// Describe implements machine.Driver. It states the process and what it needs
// open, with the digests of the pinned binary, kernel and initrd for the supervisor to
// check; it reads neither file. It keeps nothing: Boot is given the boot
// description again.
func (d *Driver) Describe(_ context.Context, _ machine.MachineID, spec machine.BootSpec) (machine.ProcessDescription, error) {
	if spec.Initrd.Path != "" || spec.CommandLine != "" {
		return machine.ProcessDescription{}, fmt.Errorf("the Firecracker Driver cannot boot an OS-form Machine yet: %w", machine.ErrUnsupported)
	}
	boot := []machine.File{
		{Host: spec.Kernel.Path, In: machine.InKernel, ReadOnly: true, SHA256: spec.Kernel.SHA256},
		{Host: d.cfg.Initrd, In: machine.InInitrd, ReadOnly: true, SHA256: d.cfg.InitrdSHA256},
	}
	return d.process(spec, append(boot, machine.DeviceFiles(spec.Devices)...))
}

// process states the VMM process for a boot description, given the files the
// VMM is to see: those of a boot, or those of a restore.
func (d *Driver) process(spec machine.BootSpec, files []machine.File) (machine.ProcessDescription, error) {
	if err := validate(spec); err != nil {
		return machine.ProcessDescription{}, err
	}
	nodes := []string{"/dev/kvm"}
	if spec.Network.Tap != "" {
		nodes = append(nodes, "/dev/net/tun")
	}
	return machine.ProcessDescription{
		Executable:       d.cfg.Binary,
		ExecutableSHA256: d.cfg.BinarySHA256,
		Args:             []string{"--api-sock", vmmconfig.InRun(vmmconfig.APISocket), "--enable-pci", "--log-path", vmmconfig.InRun(logName)},
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
