package cloudhypervisor

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
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
	// netID names the network device, so that a restore's net_fds says which
	// device takes the new tap.
	netID = "net0"
	// eventDescriptor is the descriptor the supervisor passes the write end of
	// the VMM's event pipe as: the one above the console's (ADR 0026).
	eventDescriptor = 7
)

// rebootingEvent is what the VMM's event monitor writes when the guest has
// reset, however it did: v54.0 writes each event as indented JSON, and the
// event of a reset is {"source": "vm", "event": "rebooting"}. The supervisor
// ends the VMM when it reads this (ADR 0026).
var rebootingEvent = []byte(`"event": "rebooting"`)

// cmdline is the kernel command line: the serial console the VMM carries to the
// run directory's log, and the guest parameters that make a panic reset the
// guest through the keyboard controller (ADR 0026). x86_64's serial port is
// ttyS0 and aarch64's PL011 is ttyAMA0 (docs/device_model.md). The i8042
// parameters, x86_64's, are the Firecracker Driver's. On x86_64 the guest onlines
// hot-plugged memory to ZONE_MOVABLE within the auto-movable policy's ratio, so
// that a resize can unplug it again (ADR 0029). The guest ends its Machine by
// power off on both architectures: the VMM ends on a guest shutdown and only
// reboots on a reset, which the supervisor ends (ADR 0026, ADR 0032).
func cmdline(goarch string) string {
	if goarch == "arm64" {
		return "console=ttyAMA0 reboot=k panic=1 mvmd.end=poweroff"
	}
	return "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd memhp_default_state=online memory_hotplug.online_policy=auto-movable mvmd.end=poweroff"
}

// osCmdline is the kernel command line of an OS-form boot: the serial console
// and the reset parameters of cmdline, which the i8042 ones follow on x86_64,
// then the client's line. It carries neither the hot-plug policy, since a
// resize needs the Guest Agent, nor mvmd.end, which only the agent reads
// (ADR 0059).
func osCmdline(goarch, client string) string {
	params := "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
	if goarch == "arm64" {
		params = "console=ttyAMA0 reboot=k panic=1"
	}
	return machine.OSCommandLine(params, client)
}

// kernelCmdline is the command line a boot gives the guest.
func kernelCmdline(goarch string, spec machine.BootSpec) string {
	if spec.OSForm() {
		return osCmdline(goarch, spec.CommandLine)
	}
	return cmdline(goarch)
}

// Describe implements machine.Driver. It states the process, whose arguments
// are the VM's whole configuration, and what it needs open, with the digests of
// the pinned binary and kernel for the supervisor to check; it reads neither
// file. It keeps nothing: Boot is given the boot description again.
func (d *Driver) Describe(_ context.Context, _ machine.MachineID, spec machine.BootSpec) (machine.ProcessDescription, error) {
	if err := validate(spec); err != nil {
		return machine.ProcessDescription{}, err
	}
	if n := len(kernelCmdline(d.goarch, spec)); n > machine.MaxKernelCommandLine {
		return machine.ProcessDescription{}, fmt.Errorf("the kernel command line is %d bytes with the Driver's parameters, over the %d a guest kernel takes", n, machine.MaxKernelCommandLine)
	}
	initrd := d.initrd()
	if false {
		initrd = machine.File{Host: spec.Initrd.Path, In: machine.InInitrd, ReadOnly: true, SHA256: spec.Initrd.SHA256}
	}
	boot := []machine.File{
		{Host: spec.Kernel.Path, In: machine.InKernel, ReadOnly: true, SHA256: spec.Kernel.SHA256},
		initrd,
	}
	return d.process(spec, args(spec, d.goarch), boot), nil
}

// initrd is the initrd file of a description, the same at boot and on restore.
func (d *Driver) initrd() machine.File {
	return machine.File{Host: d.cfg.Initrd, In: machine.InInitrd, ReadOnly: true, SHA256: d.cfg.InitrdSHA256}
}

// process is the description of the VMM process with the arguments and the boot
// files of a start or of a restore, and the Machine's devices after them.
func (d *Driver) process(spec machine.BootSpec, args []string, boot []machine.File) machine.ProcessDescription {
	return machine.ProcessDescription{
		Executable:       d.cfg.Binary,
		ExecutableSHA256: d.cfg.BinarySHA256,
		Args:             args,
		BootFiles:        append(boot, machine.DeviceFiles(spec.Devices)...),
		ReadFiles:        cpuReadFiles(runtime.GOARCH),
		// /dev/urandom is the source of the virtio-rng device Cloud Hypervisor
		// adds by default; without it the VM does not boot.
		DeviceNodes:     []string{"/dev/kvm", "/dev/urandom"},
		TapByDescriptor: spec.Network.Tap != "",
		EndOn:           machine.PipeTrigger{FD: eventDescriptor, Match: rebootingEvent},
		Overhead: machine.ProcessOverhead{
			MemoryBytes: overheadMemory,
			Tasks:       ceiling(spec).VCPUs + overheadTasks + uint32(len(spec.Devices)),
			CPUs:        overheadCPUs,
		},
	}
}

// args is the command line that configures the VM. Paths are those in the VMM's
// root, and the guest's serial console is the VMM's standard output and input
// with no virtio console beside it. The event monitor writes to the pipe the
// supervisor passes as a descriptor. The network device is the tap the
// supervisor passes as a descriptor: the API takes it by name only.
func args(spec machine.BootSpec, goarch string) []string {
	a := []string{
		"--api-socket", "path=" + vmmconfig.InRun(vmmconfig.APISocket),
		"--log-file", vmmconfig.InRun(logName),
		"--kernel", machine.InKernel,
		"--initramfs", machine.InInitrd,
		"--cmdline", kernelCmdline(goarch, spec),
		"--cpus", cpusArg(spec, goarch),
		"--memory", memoryArg(spec, goarch),
		"--serial", "tty",
		"--console", "off",
		"--event-monitor", fmt.Sprintf("fd=%d", eventDescriptor),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", vmmconfig.GuestCID, vmmconfig.InRun(vmmconfig.VsockSocket)),
	}
	if len(spec.Devices) > 0 {
		a = append(a, "--disk")
	}
	for i, dev := range spec.Devices {
		ro := "off"
		if dev.ReadOnly {
			ro = "on"
		}
		a = append(a, fmt.Sprintf("path=%s,readonly=%s,image_type=raw,id=%s", machine.InDisk(i), ro, diskdevice.Name(i, dev.Disk)))
	}
	if spec.Network.Tap != "" {
		a = append(a, "--net", fmt.Sprintf("id=%s,fd=[%d],mac=%s", netID, tapDescriptor, spec.Network.MAC))
	}
	return a
}

// ceiling is the figures the VM is configured to grow to: the Machine's
// ceiling, each figure it leaves unset being the resources (ADR 0029).
func ceiling(spec machine.BootSpec) machine.Resources {
	return spec.Ceiling.OrElse(spec.Resources)
}

// cpusArg is the --cpus value: the vCPUs booted and, where a resize is
// possible, the most it can bring up. aarch64 has no resize (Info), so its VM
// is configured as it was before ceilings.
func cpusArg(spec machine.BootSpec, goarch string) string {
	a := fmt.Sprintf("boot=%d", spec.Resources.VCPUs)
	if resizeCapable(goarch) {
		a += fmt.Sprintf(",max=%d", ceiling(spec).VCPUs)
	}
	return a
}

// memoryArg is the --memory value: the memory at boot and, when a resize is
// possible and the ceiling is above it, a virtio-mem region the size of the
// difference, which a resize plugs into and out of. A ceiling equal to the
// resources adds no device.
func memoryArg(spec machine.BootSpec, goarch string) string {
	a := fmt.Sprintf("size=%dM", spec.Resources.MemoryBytes/mib)
	if extra := ceiling(spec).MemoryBytes - spec.Resources.MemoryBytes; extra > 0 && resizeCapable(goarch) {
		a += fmt.Sprintf(",hotplug_method=virtio-mem,hotplug_size=%dM", extra/mib)
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
	case ceiling(spec).VCPUs < spec.Resources.VCPUs || ceiling(spec).VCPUs > maxVCPUs:
		return fmt.Errorf("a ceiling of %d vCPUs is outside %d to %d", ceiling(spec).VCPUs, spec.Resources.VCPUs, maxVCPUs)
	case ceiling(spec).MemoryBytes < spec.Resources.MemoryBytes || ceiling(spec).MemoryBytes%mib != 0:
		return fmt.Errorf("a ceiling of %d bytes of memory is below the resources or not a whole number of MiB", ceiling(spec).MemoryBytes)
	case len(spec.Devices) == 0 && !spec.OSForm():
		return fmt.Errorf("a Machine boots with at least one block device")
	}
	for _, dev := range spec.Devices {
		if !filepath.IsAbs(dev.Path) {
			return fmt.Errorf("the block device path %q is not absolute", dev.Path)
		}
	}
	return nil
}
