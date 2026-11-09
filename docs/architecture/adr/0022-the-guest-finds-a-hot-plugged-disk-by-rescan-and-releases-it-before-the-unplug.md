# ADR 0022: The guest finds a hot-plugged disk by rescan and releases it before the unplug

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#122, from the Driver's disk attach and detach (garamsh/mvmd#109, PR #120). The Driver puts a disk on a running VMM and takes one off, and tells the guest nothing ([driver.md](../driver.md) §Disk attach and detach). Firecracker v1.17.0's `docs/device-hotplug.md` says what that leaves to the guest:

- Device hotplug is a developer preview and needs the VMM started with `--enable-pci`.
- Firecracker does not deliver a hotplug notification to the guest. The guest discovers an added device by `echo 1 > /sys/bus/pci/rescan`.
- Before the host removes a device the guest must release it: unmount it, then `echo 1 > /sys/bus/pci/devices/<BDF>/remove`. Only then does the host send `DELETE /drives/<id>`.

So a guest that is not told neither sees an attached disk nor lets go of a detached one, and a detach under a mounted filesystem is a surprise removal.

Forces:

- The pinned Kata Containers 4.3.1 kernel 6.18.56 builds `CONFIG_PCI`, `CONFIG_VIRTIO_PCI`, `CONFIG_VIRTIO_BLK`, `CONFIG_SYSFS`, `CONFIG_DEVTMPFS` and, for hotplug, `CONFIG_HOTPLUG_PCI_PCIE`, `CONFIG_HOTPLUG_PCI_ACPI` and `CONFIG_HOTPLUG_PCI_SHPC`, all `=y` in `config-6.18.56-208` (amd64). The kernel could act on a hotplug event, but Firecracker sends none, so something in the guest has to ask: that is the agent.
- A boot-time disk is found by filesystem UUID ([ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)). mvmd never reads a guest disk and a disk's content is the guest's ([ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)), so a hot-plugged disk has no UUID mvmd knows.
- Firecracker builds a virtio-blk device's ID from the backing file's `st_dev`, `st_rdev` and `st_ino` (`build_device_id` in `src/vmm/src/devices/virtio/block/virtio/device.rs` at v1.17.0), and the `Drive` schema of `src/firecracker/swagger/firecracker.yaml` has no serial field. Neither a serial nor the drive ID can be chosen or is visible to the guest, which is ADR 0010's finding for boot disks as well.
- What the guest does with a disk beyond finding it is open (garamsh/discussions#2 §6 item 9, [runtime.md](../runtime.md) §Open questions).
- The guest can refuse (the disk is in use) or fail to answer, and the host must not remove a device under a mounted filesystem.

Candidates weighed for finding and identifying:

- **A rescan the agent performs on a call, identifying the new device by appearance** — chosen.
- **Waiting for the kernel's hotplug event.** Firecracker sends none (device-hotplug.md, v1.17.0).
- **Identifying by serial or drive ID.** Not choosable on Firecracker, as above.
- **Identifying by filesystem UUID.** mvmd knows none and never reads the disk.
- **Finding disks by order.** Rejected in ADR 0010 for boot disks, for the reason that applies here: attach order is not device order.

## Decision

- **Two calls on the Guest Agent, additions to `v1alpha1`:** `FindDisk` and `ReleaseDisk`, each naming the disk by the ID mvmd attached it under (`guest-agent/proto/mvmd/guest/v1alpha1/guest.proto`). They act on the kernel's device tree, which the agent has before it listens, and the daemon sends them only to a Machine that is Running, so they are served before `Boot` has succeeded, like `Terminate`. `guest-agent-channel` refuses them as a failed precondition, since it is not the guest and its `/sys` is the host's.
- **`FindDisk` writes `1` to `/sys/bus/pci/rescan` and takes the virtio block device that appeared.** The agent lists the virtio block devices in `/sys/class/block` before, rescans, and waits up to a bound for exactly one new one. It records the disk ID against the device's PCI address, read from the canonical path of its `/sys/class/block` entry, and returns the device name, its size and whether it is read-only. Calls for one Machine are serialised, so two attaches cannot be mistaken for each other's device. A repeat for a recorded ID returns the record without rescanning. Two or more new devices, or none within the bound, fail the call and record nothing.
- **What the guest does with the device is nothing.** It does not mount, format or partition it, and its device node is left for the entrypoint. This stops at what #2 §6 item 9 leaves open and does not decide it.
- **`ReleaseDisk` refuses while the device is in use, and otherwise removes it.** It refuses as a failed precondition with a `DiskInUse` status detail naming the disk when the device or one of its partitions is the source or device number of a mount in any process's mount table, or has holders in `/sys/class/block/<name>/holders`. It then writes `1` to `/sys/bus/pci/devices/<address>/remove`, waits up to a bound for the block device to be gone, and forgets the record. A disk ID the agent has not recorded is `NOT_FOUND`. A repeat after a release is `NOT_FOUND`, which the daemon reads as released.
- **The daemon orders the calls around the Driver's.** Attach on a Running Machine records the holder, calls the Driver, then `FindDisk`; a failure of the guest step detaches again through the Driver and releases the holder, so the state after failure is not attached. Detach calls `ReleaseDisk`, then the Driver; a refusal is a precondition failure and the disk stays attached. A guest that does not answer or fails fails the Detach closed: the disk stays attached, and a client who needs it gone stops the Machine, after which detach involves no guest. A Driver detach that fails after a release asks the guest to find the disk again, best effort, so the guest and the VMM agree.
- **A Paused Machine's attach and detach are refused as a precondition failure.** A paused guest cannot answer, and an attach the guest never sees, or a detach it never released, is the case this decision exists to prevent. A Machine that is Created, Stopped or Faulted has no guest: attach and detach change the boot description and the record only, as before.

## Consequences

- A running guest sees an attached disk without a reboot, and a disk the guest has mounted is not unplugged from under it.
- The identification is by appearance, so it is sound only while the calls for a Machine are serialised and the kernel shows one new device per attach. A device that appears for another reason during the bound would be taken for the disk.
- The in-use check reads mount tables, holders and device numbers, not open file descriptors, and misses a filesystem on an anonymous device number such as btrfs's. A process that holds the raw device open does not stop a release.
- A disk the Machine booted with has no recorded ID, so it is not detachable while the Machine is Running; stopping the Machine first is the way. Whether the agent should find boot-given non-root disks, and by what, is left open.
- The Guest Agent's contract grows by two calls and one detail, the daemon's generated Go client changes with it, and the Guest port gains two operations every implementation of it serves. The Machine lifecycle now depends on the guest for a hot attach and detach, where it depended on the Driver alone.
- The kernel conditions gain `CONFIG_PCI`, `CONFIG_VIRTIO_PCI` and the sysfs `rescan` and `remove` files, and the VMM must run with `--enable-pci`.

## Signs the decision was wrong

- A real guest sees two new devices for one attach, or the new device is not the attached disk, in the real test or on a node.
- Firecracker, or a later VMM Driver, begins delivering a hotplug notification and the kernel's own handling replaces the rescan.
- Detaches fail often because the guest does not answer, and clients stop Machines to detach: the fail-closed rule is too strict.
- Item 9 is decided with an identity for a non-root disk (a UUID or a name), which would let the agent identify the device by it and drop the appearance rule.
