package cloudhypervisor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/pinnedfile"
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
// parameters, x86_64's, are the Firecracker Driver's.
func cmdline(goarch string) string {
	if goarch == "arm64" {
		return "console=ttyAMA0 reboot=k panic=1"
	}
	return "console=ttyS0 reboot=k panic=1 i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
}

// verify refuses a binary or a kernel that is not the one the node pinned. It
// is how a node fails at its start and not at its first boot; the check of
// record is the supervisor's, on the file it presents to the VMM (ADR 0019).
func verify(cfg Config) error {
	for _, f := range []struct{ name, path, want string }{
		{"binary", cfg.Binary, cfg.BinarySHA256},
		{"kernel", cfg.Kernel, cfg.KernelSHA256},
	} {
		if err := pinnedfile.Check("the cloud-hypervisor "+f.name, f.path, f.want); err != nil {
			return err
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
		d.initrd(),
	}
	return d.process(spec, args(spec, runtime.GOARCH), boot), nil
}

// initrd is the initrd file of a description, the same at boot and on restore.
func (d *Driver) initrd() machine.File {
	return machine.File{Host: d.cfg.Initrd, In: machine.InInitrd, ReadOnly: true}
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
			Tasks:       spec.Resources.VCPUs + overheadTasks + uint32(len(spec.Devices)),
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
		"--api-socket", "path=" + inRun(apiSocketName),
		"--log-file", inRun(logName),
		"--kernel", machine.InKernel,
		"--initramfs", machine.InInitrd,
		"--cmdline", cmdline(goarch),
		"--cpus", "boot=" + strconv.FormatUint(uint64(spec.Resources.VCPUs), 10),
		"--memory", "size=" + strconv.FormatUint(spec.Resources.MemoryBytes/mib, 10) + "M",
		"--serial", "tty",
		"--console", "off",
		"--event-monitor", fmt.Sprintf("fd=%d", eventDescriptor),
		"--vsock", fmt.Sprintf("cid=%d,socket=%s", guestCID, inRun(vsockSocketName)),
		"--disk",
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
