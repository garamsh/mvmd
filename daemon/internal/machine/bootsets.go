package machine

import (
	"fmt"
	"slices"
	"strings"
)

// BootSet is a kernel and an initrd the node offers an OS-form Machine by name,
// each pinned by digest (ADR 0058).
type BootSet struct {
	Kernel KernelFile
	Initrd KernelFile
}

// BootSets are the boot sets a node offers, each by name (ADR 0058).
type BootSets map[string]BootSet

// Names lists the offered names in order.
func (b BootSets) Names() []string {
	names := make([]string, 0, len(b))
	for name := range b {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// BootFileChecker states whether a file on the host still has the digest the
// node pinned for it. what names the file in the error.
type BootFileChecker interface {
	Check(what string, file KernelFile) error
}

// OSBoot is the OS form of a Machine: it boots the named boot set with the
// command line, and has no image, no root disk and no Guest Agent (ADR 0058).
type OSBoot struct {
	BootSet string
	// CommandLine is what the client adds to the Driver's own parameters.
	CommandLine string
}

// maxCommandLine is the longest kernel command line a client may give, the
// size both kernels' architectures accept.
const maxCommandLine = 2048

func (o OSBoot) validate() error {
	switch {
	case o.BootSet == "":
		return fail(ErrInvalid, "os_boot.boot_set is empty")
	case len(o.CommandLine) > maxCommandLine:
		return fail(ErrInvalid, "os_boot.kernel_cmdline is longer than %d bytes", maxCommandLine)
	case strings.ContainsAny(o.CommandLine, "\x00\n"):
		return fail(ErrInvalid, "os_boot.kernel_cmdline holds a NUL or a newline")
	}
	return nil
}

// validateOS refuses each field of the image form that an OS-form request
// sets, naming it, and a mount path, which no agent would act on.
func (s Spec) validateOS() error {
	set := []struct {
		field string
		set   bool
	}{
		{"image_digest", s.Image != ""},
		{"root_disk_bytes", s.RootDiskBytes != 0},
		{"kernel", s.Kernel != ""},
		{"command", len(s.Command) > 0},
		{"args", len(s.Args) > 0},
		{"env", len(s.Env) > 0},
		{"user", s.User != ""},
		{"files", len(s.Files) > 0},
		{"working_dir", s.WorkingDir != ""},
		{"probes", len(s.Probes) > 0},
	}
	for _, f := range set {
		if f.set {
			return fail(ErrInvalid, "%s is set, and an OS-form Machine has none", f.field)
		}
	}
	for _, a := range s.Disks {
		if a.MountPath != "" {
			return fail(ErrInvalid, "disk %q has a mount path, and an OS-form Machine has no agent to mount it", a.Disk)
		}
	}
	return s.OS.validate()
}

// requireAgent refuses a call that needs the Guest Agent on an OS-form Machine.
func (m Machine) requireAgent(call string) error {
	if m.OS != nil {
		return fail(ErrPreconditionFailed, "%s needs the Guest Agent, and Machine %q is an OS-form Machine, which has none", call, m.ID)
	}
	return nil
}

// requireBootTime refuses a change of an OS-form Machine's disks while it has a
// VMM: its disks are drives of its boot, and nothing hot-plugs them (ADR 0058).
func (m Machine) requireBootTime(call string) error {
	if m.OS != nil && m.State == StatePaused+100 {
		return fail(ErrPreconditionFailed, "%s: Machine %q is %s, and an OS-form Machine's disks change only while it is Created or Stopped", call, m.ID, m.State)
	}
	return nil
}

// bootSet resolves the name an OS-form Machine records.
func (s *service) bootSet(o OSBoot) (BootSet, *Error) {
	set, ok := s.bootSets[o.BootSet]
	if !ok {
		return BootSet{}, fail(ErrInvalid, "boot set %q is not offered by the node", o.BootSet)
	}
	return set, nil
}

// checkBootSet states that both files of the set still have their pinned
// digests, naming the one that has not.
func (s *service) checkBootSet(name string, set BootSet) error {
	if s.bootFiles == nil {
		return nil
	}
	for _, f := range []struct {
		what string
		file KernelFile
	}{{"kernel", set.Kernel}, {"initrd", set.Initrd}} {
		if err := s.bootFiles.Check(fmt.Sprintf("the %s of boot set %q", f.what, name), f.file); err != nil {
			return fail(ErrPreconditionFailed, "%v", err)
		}
	}
	return nil
}
