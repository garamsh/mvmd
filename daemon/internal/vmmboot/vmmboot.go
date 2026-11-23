// Package vmmboot is what both VMM Drivers state of a boot the same way: the
// figures a Machine is held to, the refusal of a boot description no VMM could
// run, the initrd an OS-form boot takes, the kernel parameters that reset the
// guest, the host files the VMM reads, and the check of the command line's
// length. A Driver keeps what is its VMM's own: its API, its process
// description and its serial device.
package vmmboot

import (
	"fmt"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	// MiB is the unit a VMM takes memory in.
	MiB = 1 << 20
	// MaxVCPUs bounds a Machine's vCPUs: the limit the Machines of every Driver
	// share, which each Driver reports in its Info (ADR 0040).
	MaxVCPUs = 32
	// OverheadMemory is added to the guest's memory for the VMM's cgroup. It is
	// the figure measured for Firecracker v1.17.0 (an idle VMM with a 256 MiB
	// guest held 60 MiB resident, and the rest covers the page tables and device
	// state a busy guest adds), and Cloud Hypervisor takes it unmeasured (ADR
	// 0015: the peak, not the average).
	OverheadMemory = 128 * MiB
	// OverheadCPUs is the CPU the main thread takes for device emulation.
	OverheadCPUs = 1.0
	// MaxTapName is the longest interface name Linux holds: IFNAMSIZ less the NUL.
	MaxTapName = 15
)

// Validate refuses a boot description no VMM could run, before a process exists
// for it.
func Validate(spec machine.BootSpec) error {
	switch {
	case len(spec.Network.Tap) > MaxTapName:
		return fmt.Errorf("the tap name %q is %d bytes, over the %d an interface name holds", spec.Network.Tap, len(spec.Network.Tap), MaxTapName)
	case !filepath.IsAbs(spec.RunDir):
		return fmt.Errorf("the run directory %q is not an absolute path", spec.RunDir)
	case spec.Resources.VCPUs < 1 || spec.Resources.VCPUs > MaxVCPUs:
		return fmt.Errorf("%d vCPUs is outside 1 to %d", spec.Resources.VCPUs, MaxVCPUs)
	case spec.Resources.MemoryBytes == 0 || spec.Resources.MemoryBytes%MiB != 0:
		return fmt.Errorf("%d bytes of memory is not a whole number of MiB", spec.Resources.MemoryBytes)
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

// CheckCommandLine refuses a kernel command line a guest kernel would cut.
func CheckCommandLine(line string) error {
	if n := len(line); n > machine.MaxKernelCommandLine {
		return fmt.Errorf("the kernel command line is %d bytes with the Driver's parameters, over the %d a guest kernel takes", n, machine.MaxKernelCommandLine)
	}
	return nil
}

// Initrd is the initrd file of a boot: the one the OS-form boot names, in place
// of the one the node configures (ADR 0058).
func Initrd(spec machine.BootSpec, configuredHost, configuredSHA256 string) machine.File {
	if spec.OSForm() {
		return machine.File{Host: spec.Initrd.Path, In: machine.InInitrd, ReadOnly: true, SHA256: spec.Initrd.SHA256}
	}
	return machine.File{Host: configuredHost, In: machine.InInitrd, ReadOnly: true, SHA256: configuredSHA256}
}

// ResetParams are the kernel parameters that make a panic reset the guest, which
// ends the VMM. i8042's probe parameters are the ones the Firecracker
// documentation gives to skip the probe, and apply on x86_64 only (ADR 0026).
func ResetParams(goarch string) string {
	params := "reboot=k panic=1"
	if goarch == "amd64" {
		return params + " i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd"
	}
	return params
}

// CPUCacheFiles are the host files the VMM reads beyond the boot files on
// aarch64: the CPU cache description (ADR 0015). There are none elsewhere.
func CPUCacheFiles(goarch string) []string {
	if goarch != "arm64" {
		return nil
	}
	return []string{"/sys/devices/system/cpu/cpu0/cache"}
}
