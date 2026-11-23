package firecracker

import (
	"context"
	"runtime"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmboot"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
)

const (
	// overheadTasks is the VMM's tasks beside its vCPUs: its main and API
	// threads, the kernel's NX huge page recovery thread KVM starts for it, and
	// a margin. Devices run on the main thread with the Sync disk engine.
	overheadTasks = 8
)

// bootParams are the kernel parameters the Driver puts first: the serial
// console the VMM carries to the run directory's log, and a reset that ends the
// VMM.
func bootParams(goarch string) string {
	return "console=ttyS0 " + vmmboot.ResetParams(goarch)
}

// bootArgs is the kernel command line of an image-form boot: bootParams and
// the method the guest ends its Machine by (ADR 0032). On x86_64 the VMM ends
// on the keyboard-controller reset, and a power off halts the guest with the
// VMM running. On aarch64 PSCI turns both into a KVM system event the VMM ends
// on, so the plain power off is used.
func bootArgs(goarch string) string {
	if goarch == "amd64" {
		return bootParams(goarch) + " mvmd.end=reset"
	}
	return bootParams(goarch) + " mvmd.end=poweroff"
}

// osBootArgs is the kernel command line of an OS-form boot: bootParams, then
// the client's line. No Guest Agent reads mvmd.end (ADR 0059).
func osBootArgs(goarch, client string) string {
	return machine.OSCommandLine(bootParams(goarch), client)
}

// kernelArgs is the command line a boot gives the guest.
func kernelArgs(goarch string, spec machine.BootSpec) string {
	if spec.OSForm() {
		return osBootArgs(goarch, spec.CommandLine)
	}
	return bootArgs(goarch)
}

// Describe implements machine.Driver. It states the process and what it needs
// open, with the digests of the pinned binary, kernel and initrd for the supervisor to
// check; it reads neither file. It keeps nothing: Boot is given the boot
// description again.
func (d *Driver) Describe(_ context.Context, _ machine.MachineID, spec machine.BootSpec) (machine.ProcessDescription, error) {
	boot := []machine.File{
		{Host: spec.Kernel.Path, In: machine.InKernel, ReadOnly: true, SHA256: spec.Kernel.SHA256},
		vmmboot.Initrd(spec, d.cfg.Initrd, d.cfg.InitrdSHA256),
	}
	return d.process(spec, append(boot, machine.DeviceFiles(spec.Devices)...))
}

// process states the VMM process for a boot description, given the files the
// VMM is to see: those of a boot, or those of a restore.
func (d *Driver) process(spec machine.BootSpec, files []machine.File) (machine.ProcessDescription, error) {
	if err := vmmboot.Validate(spec); err != nil {
		return machine.ProcessDescription{}, err
	}
	if err := vmmboot.CheckCommandLine(kernelArgs(runtime.GOARCH, spec)); err != nil {
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
			MemoryBytes: vmmboot.OverheadMemory,
			Tasks:       spec.Resources.VCPUs + overheadTasks,
			CPUs:        vmmboot.OverheadCPUs,
		},
	}, nil
}

// cpuReadFiles are the host files the VMM reads beyond the boot files: on
// aarch64 the CPU cache description and the MIDR register (ADR 0015).
func cpuReadFiles(goarch string) []string {
	files := vmmboot.CPUCacheFiles(goarch)
	if files == nil {
		return nil
	}
	return append(files, "/sys/devices/system/cpu/cpu0/regs/identification/midr_el1")
}
