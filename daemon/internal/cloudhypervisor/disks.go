package cloudhypervisor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ejectWait bounds how long DetachDisk waits for the guest to eject a device.
const ejectWait = 10 * time.Second

// ejectPoll is how often DetachDisk asks whether the device is gone.
const ejectPoll = 50 * time.Millisecond

// driveID names a block device to the VMM. A device that holds a disk is named
// by it, so a detach finds the device whether the disk was given at boot or
// attached later, and a Machine's image, which holds none, by its position. The
// name is also a file name in the run directory, so it is the hex of the ID's
// SHA-256: the same length for any ID, free of the "__" prefix Cloud
// Hypervisor reserves for its own, and distinct for two IDs. It is the name the
// Firecracker Driver gives a drive.
func driveID(position int, disk machine.DiskID) string {
	if disk == "" {
		return fmt.Sprintf("disk%d", position)
	}
	sum := sha256.Sum256([]byte(disk))
	return "disk_" + hex.EncodeToString(sum[:])
}

// AttachDisk implements machine.Driver. The VMM opens what is in its root,
// which the supervisor fixed at its start, so the disk's file is hard linked
// into the run directory, the one place mvmd adds files to a VMM that is
// running (ADR 0015), and the device is added at that path (PUT /vm.add-disk).
// It is a virtio-pci device, and the VMM tells the guest of it by an ACPI
// notification (docs/hotplug.md); what the guest does with that is the guest
// agent's (ADR 0022).
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
	body := map[string]any{"id": name, "path": inRun(name), "readonly": dev.ReadOnly, "image_type": "Raw"}
	if err := newClient(filepath.Join(dir, apiSocketName)).do(ctx, "PUT", "/vm.add-disk", body, nil); err != nil {
		return errors.Join(err, removeLink(link))
	}
	return nil
}

// DetachDisk implements machine.Driver. It asks the VMM to remove the device
// (PUT /vm.remove-device) unless the VMM's configuration no longer holds it,
// waits until the VMM has let go of it, then removes the link an attach made.
// The VMM drops the device from its configuration at once but keeps its
// identifier until the guest has ejected it, and an add of the same identifier
// before then is refused, so the wait is what lets a disk be attached again at
// once. A request for a device the configuration lacks is refused, so a retry
// of a detach whose request was accepted waits again and sends nothing. A disk
// given at boot has no link, and its path stays bound in the root.
func (d *Driver) DetachDisk(ctx context.Context, id machine.MachineID, disk machine.DiskID) error {
	if disk == "" {
		return errors.New("a disk to detach names no disk")
	}
	dir, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return err
	}
	name := driveID(0, disk)
	c := newClient(filepath.Join(dir, apiSocketName))
	info, err := c.info(ctx)
	if err != nil {
		return err
	}
	if info.holdsDisk(name) {
		if err := c.do(ctx, "PUT", "/vm.remove-device", map[string]string{"id": name}, nil); err != nil {
			return err
		}
	}
	err = nil
	// The VMM no longer holds the file, ejected or not.
	return errors.Join(err, removeLink(filepath.Join(dir, name)))
}

// awaitEjected polls the VMM's device tree until it has no device of the name.
func awaitEjected(ctx context.Context, c *client, name string) error {
	ctx, cancel := context.WithTimeout(ctx, ejectWait)
	defer cancel()
	for {
		info, err := c.info(ctx)
		if err != nil {
			return fmt.Errorf("wait for the guest to eject %s: %w", name, err)
		}
		if _, held := info.Tree[name]; !held {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the guest did not eject %s: %w", name, ctx.Err())
		case <-time.After(ejectPoll):
		}
	}
}

// Drive implements machine.Driver. It reads the VMM's own configuration
// (GET /vm.info) and finds the device of the disk by its name: a device at its
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
	info, err := c.info(ctx)
	if err != nil {
		return machine.DriveAbsent, err
	}
	name := driveID(0, disk)
	for _, have := range info.Config.Disks {
		switch {
		case have.ID != name:
		case have.Path == inRun(name):
			return machine.DriveHotPlugged, nil
		default:
			return machine.DriveAtBoot, nil
		}
	}
	return machine.DriveAbsent, nil
}

// vmInfo is the part of GET /vm.info the disk calls read: the disks the VMM's
// configuration holds, and its device tree, which keeps a removed device's
// identifier until the guest has ejected it.
type vmInfo struct {
	Config struct {
		Disks []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"disks"`
	} `json:"config"`
	Tree map[string]json.RawMessage `json:"device_tree"`
}

func (c *client) info(ctx context.Context) (vmInfo, error) {
	var info vmInfo
	err := c.do(ctx, "GET", "/vm.info", nil, &info)
	return info, err
}

func (i vmInfo) holdsDisk(name string) bool {
	for _, have := range i.Config.Disks {
		if have.ID == name {
			return true
		}
	}
	return false
}

// removeLink removes the link of an attached disk, which is absent for a disk
// given at boot.
func removeLink(link string) error {
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", link, err)
	}
	return nil
}
