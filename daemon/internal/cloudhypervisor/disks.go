package cloudhypervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/vmmconfig"
)

// ejectWait bounds how long DetachDisk waits for the guest to eject a device.
const ejectWait = 10 * time.Second

// ejectPoll is how often DetachDisk asks whether the device is gone.
const ejectPoll = 50 * time.Millisecond

// AttachDisk implements machine.Driver. The VMM opens what is in its root,
// which the supervisor fixed at its start, so the disk's file is hard linked
// into the run directory, the one place mvmd adds files to a VMM that is
// running (ADR 0015), and the device is added at that path (PUT /vm.add-disk).
// It is a virtio-pci device, and the VMM tells the guest of it by an ACPI
// notification (docs/hotplug.md); what the guest does with that is the guest
// agent's (ADR 0022).
func (d *Driver) AttachDisk(ctx context.Context, id machine.MachineID, dev machine.BlockDevice) error {
	dir, err := diskdevice.Link(d.cfg.Dirs, id, dev)
	if err != nil {
		return err
	}
	name := diskdevice.Name(0, dev.Disk)
	link := filepath.Join(dir, name)
	body := map[string]any{"id": name, "path": vmmconfig.InRun(name), "readonly": dev.ReadOnly, "image_type": "Raw"}
	if err := newClient(filepath.Join(dir, vmmconfig.APISocket)).do(ctx, "PUT", "/vm.add-disk", body, nil); err != nil {
		return errors.Join(err, diskdevice.RemoveLink(link))
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
	name := diskdevice.Name(0, disk)
	c := newClient(filepath.Join(dir, vmmconfig.APISocket))
	info, err := c.info(ctx)
	if err != nil {
		return err
	}
	if info.holdsDisk(name) {
		if err := c.do(ctx, "PUT", "/vm.remove-device", map[string]string{"id": name}, nil); err != nil {
			return err
		}
	}
	err = awaitEjected(ctx, c, name)
	// The VMM no longer holds the file, ejected or not.
	return errors.Join(err, diskdevice.RemoveLink(filepath.Join(dir, name)))
}

// awaitEjected polls the VMM's device tree until it has no device of the name.
// A wait whose context ends reports that the guest did not eject, whether the
// end fell between two polls or cut one short; any other failure of a poll is
// reported as itself.
func awaitEjected(ctx context.Context, c *client, name string) error {
	ctx, cancel := context.WithTimeout(ctx, ejectWait)
	defer cancel()
	for {
		info, err := c.info(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("the guest did not eject %s: %w", name, ctx.Err())
			}
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
	name := diskdevice.Name(0, disk)
	for _, have := range info.Config.Disks {
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

// ResizeDisk implements machine.Driver. It asks the VMM to resize the device of
// the disk (PUT /vm.resize-disk): the VMM sets the length of the raw file
// itself, pauses the device while it updates its capacity, and sends the guest
// a configuration-change interrupt (docs/api.md and the v54.0 release notes,
// "Live Disk Resizing Support for Raw Images"). The file is therefore opened
// read-write, and a read-only attachment is refused by the VMM. The VMM
// requires a multiple of 512 bytes, which is checked here so nothing is sent
// for a size it would refuse.
func (d *Driver) ResizeDisk(ctx context.Context, id machine.MachineID, disk machine.DiskID, sizeBytes uint64) error {
	if disk == "" {
		return errors.New("a disk to resize names no disk")
	}
	if sizeBytes == 0 || sizeBytes%512 != 0 {
		return fmt.Errorf("cannot resize %q to %d bytes: Cloud Hypervisor v54.0 resizes a disk to a multiple of 512 bytes", disk, sizeBytes)
	}
	c, err := d.api(id)
	if err != nil {
		return err
	}
	body := map[string]any{"id": diskdevice.Name(0, disk), "desired_size": sizeBytes}
	return c.do(ctx, "PUT", "/vm.resize-disk", body, nil)
}

// vmInfo is the part of GET /vm.info the disk and resize calls read: the disks
// the VMM's configuration holds, its device tree, which keeps a removed
// device's identifier until the guest has ejected it, and the vCPUs and memory
// it is configured with and the memory the guest has plugged.
type vmInfo struct {
	Config struct {
		CPUs struct {
			BootVCPUs uint32 `json:"boot_vcpus"`
		} `json:"cpus"`
		Memory struct {
			Size           uint64 `json:"size"`
			HotpluggedSize uint64 `json:"hotplugged_size"`
		} `json:"memory"`
		Disks []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"disks"`
	} `json:"config"`
	Tree             map[string]json.RawMessage `json:"device_tree"`
	MemoryActualSize uint64                     `json:"memory_actual_size"`
}

// configuredMemory is the memory the VMM is configured to give the guest: what
// it booted with and what a resize has since asked to be plugged.
func (i vmInfo) configuredMemory() uint64 {
	return i.Config.Memory.Size + i.Config.Memory.HotpluggedSize
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
