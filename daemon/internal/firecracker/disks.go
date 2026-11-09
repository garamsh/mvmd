package firecracker

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// driveID names a block device to the VMM. A device that holds a disk is named
// by it, so a detach finds the drive whether the disk was given at boot or
// attached later, and a Machine's image, which holds none, by its position. The
// hex keeps any disk ID within what Firecracker accepts of an ID (letters,
// digits and underscores) and two IDs apart.
func driveID(position int, disk machine.DiskID) string {
	if disk == "" {
		return fmt.Sprintf("disk%d", position)
	}
	return "disk_" + hex.EncodeToString([]byte(disk))
}

// drive is the request that configures a block device, at a path in the VMM's
// root. A read-write one has cache_type Writeback, so the guest's flushes reach
// the file; the default ignores them.
func drive(id, in string, readOnly bool) map[string]any {
	body := map[string]any{"drive_id": id, "path_on_host": in, "is_root_device": false, "is_read_only": readOnly}
	if !readOnly {
		body["cache_type"] = "Writeback"
	}
	return body
}

// AttachDisk implements machine.Driver. The VMM can open only what is in its
// root, which the supervisor fixed at its start, so the disk's file is hard
// linked into the run directory, the one place mvmd adds files to a VMM that is
// running (ADR 0015), and the drive is put at that path. It is a PCI device,
// which v1.17.0 can hot-plug, but it tells the guest nothing: the guest sees
// the disk when it rescans its PCI bus (docs/device-hotplug.md), which is not
// the Driver's to ask.
func (d *Driver) AttachDisk(ctx context.Context, id machine.MachineID, dev machine.BlockDevice) error {
	if dev.Disk == "" {
		return errors.New("a disk to attach names no disk")
	}
	if !filepath.IsAbs(dev.Path) {
		return fmt.Errorf("the disk file %q is not an absolute path", dev.Path)
	}
	info, err := os.Lstat(dev.Path)
	if err != nil {
		return fmt.Errorf("the disk file: %w", err)
	}
	// A link made to a symbolic link would name a host path in the root.
	if !info.Mode().IsRegular() {
		return fmt.Errorf("the disk file %s is not a regular file", dev.Path)
	}
	dir, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return err
	}
	name := driveID(0, dev.Disk)
	link := filepath.Join(dir, name)
	// What stands at the link is a leftover of an attach that failed, or
	// something the VMM's user put there; neither is followed.
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear %s: %w", link, err)
	}
	if err := os.Link(dev.Path, link); err != nil {
		return fmt.Errorf("link the disk into the run directory: %w", err)
	}
	err = newClient(filepath.Join(dir, apiSocketName)).do(ctx, "PUT", "/drives/"+name, drive(name, inRun(name), dev.ReadOnly), nil)
	if err != nil {
		return errors.Join(err, removeLink(link))
	}
	return nil
}

// DetachDisk implements machine.Driver. It asks the VMM to unplug the disk's
// drive, then removes the link an attach made, so the file has no holder left
// in the run directory. The guest must have released the device first
// (docs/device-hotplug.md): v1.17.0 does not ask it to, and that is not the
// Driver's to do. A disk given at boot has no link, and its path stays bound in
// the root.
func (d *Driver) DetachDisk(ctx context.Context, id machine.MachineID, disk machine.DiskID) error {
	if disk == "" {
		return errors.New("a disk to detach names no disk")
	}
	dir, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return err
	}
	name := driveID(0, disk)
	if err := newClient(filepath.Join(dir, apiSocketName)).do(ctx, "DELETE", "/drives/"+name, nil, nil); err != nil {
		return err
	}
	return removeLink(filepath.Join(dir, name))
}

// removeLink removes the link of an attached disk, which is absent for a disk
// given at boot.
func removeLink(link string) error {
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", link, err)
	}
	return nil
}
