// Package diskdevice names the block device a VMM holds for a disk, links a
// hot-plugged disk's file into the run directory, and removes that link. Both
// VMM Drivers use it, so they name a device, make a link and clear it the same
// way.
package diskdevice

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/runlink"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
)

// Name names a block device to the VMM. A device that holds a disk is named by
// it, so a detach finds the device whether the disk was given at boot or
// attached later, and a Machine's image, which holds none, by its position. The
// name is also a file name in the run directory, so it is the hex of the ID's
// SHA-256: the same length for any ID, within the letters, digits and
// underscores Firecracker accepts of an ID, free of the "__" prefix Cloud
// Hypervisor reserves for its own, and distinct for two IDs.
func Name(position int, disk machine.DiskID) string {
	if disk == "" {
		return fmt.Sprintf("disk%d", position)
	}
	sum := sha256.Sum256([]byte(disk))
	return "disk_" + hex.EncodeToString(sum[:])
}

// RemoveLink removes the link of an attached disk, which is absent for a disk
// given at boot.
func RemoveLink(link string) error {
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", link, err)
	}
	return nil
}

// Link hard links the file of a disk to attach into the Machine's run
// directory under the disk's device name, the one place mvmd adds files to a
// VMM that is running (ADR 0015), and returns the run directory. It refuses a
// disk that names no disk and a file that is not an absolute path to a regular
// file: a link made to a symbolic link would name a host path in the root.
func Link(dirs vmmconfig.RunDirs, id machine.MachineID, dev machine.BlockDevice) (string, error) {
	if dev.Disk == "" {
		return "", errors.New("a disk to attach names no disk")
	}
	name := Name(0, dev.Disk)
	if err := runlink.Check(name, dev.Path); err != nil {
		return "", err
	}
	info, err := os.Lstat(dev.Path)
	if err != nil {
		return "", fmt.Errorf("the disk file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("the disk file %s is not a regular file", dev.Path)
	}
	dir, err := dirs.RunDir(id)
	if err != nil {
		return "", err
	}
	if err := runlink.Make(dir, name, dev.Path); err != nil {
		return "", err
	}
	return dir, nil
}

// RunFiles are the run files of the disks a restored snapshot recorded: each
// under its device's name, where the snapshot recorded it (ADR 0042).
func RunFiles(recorded []machine.BlockDevice) []machine.RunFile {
	files := make([]machine.RunFile, 0, len(recorded))
	for _, dev := range recorded {
		files = append(files, machine.RunFile{Name: Name(0, dev.Disk), Path: dev.Path})
	}
	return files
}
