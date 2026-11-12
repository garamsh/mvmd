// Package vmmconfig is what a VMM Driver's node configuration holds whichever
// the VMM: the binary, kernels and initrd it pins by SHA-256, the run
// directories it reaches Machines through, the names of the sockets a VMM
// serves in a run directory, and the checks that refuse a configuration at
// start. Both VMM Drivers use it, so a node is refused the same way by each.
package vmmconfig

import (
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/pinnedfile"
)

const (
	// APISocket and VsockSocket are the sockets a VMM serves in the run directory.
	APISocket   = "api.sock"
	VsockSocket = "vsock.sock"
	// GuestCID is the guest's vsock address; it is the same for every Machine
	// since a vsock device is private to its VMM.
	GuestCID = 3
)

// RunDirs names a Machine's run directory, where the API and vsock sockets are.
type RunDirs interface {
	RunDir(id machine.MachineID) (string, error)
}

// Files are the files a node pins for a VMM and the guest it boots (ADR 0010,
// ADR 0015). The digests are SHA-256, in hex.
type Files struct {
	Binary       string
	BinarySHA256 string
	// Kernels are the guest kernels the node offers; a boot is given the one
	// it uses in BootSpec.Kernel (ADR 0045).
	Kernels      []machine.KernelFile
	Initrd       string
	InitrdSHA256 string
}

type pinned struct{ name, path, sum string }

func (f Files) pinned() []pinned {
	files := []pinned{{"binary", f.Binary, f.BinarySHA256}, {"initrd", f.Initrd, f.InitrdSHA256}}
	for _, k := range f.Kernels {
		files = append(files, pinned{"kernel", k.Path, k.SHA256})
	}
	return files
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Validate refuses files that name no absolute path or no SHA-256 digest where
// one is due. vmm names the VMM in the error. It reads no file.
func Validate(vmm string, f Files) error {
	if len(f.Kernels) == 0 {
		return fmt.Errorf("%s lists no kernel", vmm)
	}
	for _, p := range f.pinned() {
		if !filepath.IsAbs(p.path) {
			return fmt.Errorf("%s %s %q is not an absolute path", vmm, p.name, p.path)
		}
		if !sha256Hex.MatchString(p.sum) {
			return fmt.Errorf("%s %s digest %q is not 64 lowercase hex digits", vmm, p.name, p.sum)
		}
	}
	return nil
}

// Check refuses a node that cannot start a VMM: files Validate refuses, no run
// directories, or a binary, kernel or initrd that is not the one pinned. It is
// how a node fails at its start and not at its first boot; the check of record
// is the supervisor's, on the file it presents to the VMM (ADR 0019).
func Check(vmm string, f Files, dirs RunDirs) error {
	if err := Validate(vmm, f); err != nil {
		return err
	}
	if dirs == nil {
		return fmt.Errorf("%s needs the run directories of Machines", vmm)
	}
	for _, p := range f.pinned() {
		if err := pinnedfile.Check("the "+vmm+" "+p.name, p.path, p.sum); err != nil {
			return err
		}
	}
	return nil
}

// RunPath is the host path of a file in a Machine's run directory.
func RunPath(dirs RunDirs, id machine.MachineID, name string) (string, error) {
	dir, err := dirs.RunDir(id)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, name), nil
}

// InRun is the path of a file of the run directory in the VMM's root, which is
// what the VMM is told; mvmd itself reaches the file at the run directory's
// host path.
func InRun(name string) string { return filepath.Join(machine.InRunDir, name) }
