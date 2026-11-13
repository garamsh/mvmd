package firecracker

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
)

// drive is the request that configures a block device, at a path in the VMM's
// root. A read-write one has cache_type Writeback, so the guest's flushes reach
// the file; the default ignores them.
func drive(id, in string, readOnly bool) map[string]any {
	body := map[string]any{"drive_id": id, "path_on_host": in, "is_root_device": false, "is_read_only": false}
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
	dir, err := diskdevice.Link(d.cfg.Dirs, id, dev)
	if err != nil {
		return err
	}
	name := diskdevice.Name(0, dev.Disk)
	link := filepath.Join(dir, name)
	err = newClient(filepath.Join(dir, vmmconfig.APISocket)).do(ctx, "PUT", "/drives/"+name, drive(name, vmmconfig.InRun(name), dev.ReadOnly), nil)
	if err != nil {
		return errors.Join(err, diskdevice.RemoveLink(link))
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
	name := diskdevice.Name(0, disk)
	if err := newClient(filepath.Join(dir, vmmconfig.APISocket)).do(ctx, "DELETE", "/drives/"+name, nil, nil); err != nil {
		return err
	}
	return diskdevice.RemoveLink(filepath.Join(dir, name))
}

// ResizeDisk implements machine.Driver and always refuses: Firecracker v1.17.0
// has no call that grows a drive's file, and PATCH /drives/{id}, which updates
// the emulation's length and tells the guest, is documented for a device the
// guest has not mounted and does no I/O to (docs/api_requests/patch-block.md),
// which an ext4 disk of a Machine is not. The Driver does not report the
// capability, so the lifecycle refuses the resize of a disk a Running Machine
// holds before it gets here.
func (d *Driver) ResizeDisk(context.Context, machine.MachineID, machine.DiskID, uint64) error {
	return fmt.Errorf("%w: Firecracker v1.17.0 resizes a drive only while the guest has not mounted it "+
		"(PATCH /drives/{id}, docs/api_requests/patch-block.md)", machine.ErrUnsupported)
}

// Drive implements machine.Driver. It reads the VMM's own configuration
// (GET /vm/config) and finds the drive of the disk by its name: a drive at its
// link in the run directory was added to the running VMM, and any other path
// is a device the boot gave it.
func (d *Driver) Drive(ctx context.Context, id machine.MachineID, disk machine.DiskID) (machine.DriveState, error) {
	if disk == "" {
		return machine.DriveAbsent, errors.New("a disk to look for names no disk")
	}
	c, err := d.api(id)
	if err != nil {
		return machine.DriveAbsent, err
	}
	var config struct {
		Drives []struct {
			ID   string `json:"drive_id"`
			Path string `json:"path_on_host"`
		} `json:"drives"`
	}
	if err := c.do(ctx, "GET", "/vm/config", nil, &config); err != nil {
		return machine.DriveAbsent, err
	}
	name := diskdevice.Name(0, disk)
	for _, have := range config.Drives {
		switch {
		case have.ID != name:
		case have.Path == vmmconfig.InRun(name):
			return machine.DriveHotPlugged, nil
		default:
			return machine.DriveAtBoot, nil
		}
	}
	return machine.DriveAbsent, nil
}
