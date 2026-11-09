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
- Checking that a device is unused and then removing it are two steps. Whatever mounts the device between them gets the surprise removal this decision exists to prevent.
- A detach is several steps (the guest's release, the Driver's detach, the daemon's record), and any can fail or be cut short by the daemon's death. A retry has to converge: the guest's part of it cannot be one that works once.

Candidates weighed for finding and identifying:

- **A rescan the agent performs on a call, identifying the new device by appearance** — chosen.
- **Waiting for the kernel's hotplug event.** Firecracker sends none (device-hotplug.md, v1.17.0).
- **Identifying by serial or drive ID.** Not choosable on Firecracker, as above.
- **Identifying by filesystem UUID.** mvmd knows none and never reads the disk.
- **Finding disks by order.** Rejected in ADR 0010 for boot disks, for the reason that applies here: attach order is not device order.

Candidates weighed for deciding that a device is unused:

- **An exclusive open of the block device, held across the removal** — chosen. `open(2)` with `O_EXCL` on a block device fails with `EBUSY` while the system has claimed it, and a claim is refused while the open is held, so the check and the removal cannot be separated by a mount. The kernel's own claims cover a mounted filesystem of any kind, swap and a device-mapper table, and a claimed partition makes the exclusive open of the whole device fail.
- **Scanning mount tables and holders.** Reading every process's `/proc/<pid>/mountinfo` for the device number or source of the device and its partitions, and `/sys/class/block/<name>/holders`. Rejected: it checks and then acts, so a mount between the scan and the `remove` write is not seen; it misses a filesystem on an anonymous device number such as btrfs's, and swap; and it reimplements, in more code, a check the kernel makes.

## Decision

- **Two calls on the Guest Agent, additions to `v1alpha1`:** `FindDisk` and `ReleaseDisk`, each naming the disk by the ID mvmd attached it under (`guest-agent/proto/mvmd/guest/v1alpha1/guest.proto`). They act on the kernel's device tree, which the agent has before it listens, and the daemon sends them only to a Machine that is Running, so they are served before `Boot` has succeeded, like `Terminate`. `guest-agent-channel` refuses them as a failed precondition, since it is not the guest and its `/sys` is the host's.
- **`FindDisk` writes `1` to `/sys/bus/pci/rescan` and takes the virtio block device that appeared.** The agent lists the virtio block devices in `/sys/class/block` before, rescans, and waits up to a bound for exactly one new one. It records the disk ID against the device's PCI address, read from the canonical path of its `/sys/class/block` entry, and returns the device name, its size and whether it is read-only. Calls for one Machine are serialised, so two attaches cannot be mistaken for each other's device. A repeat for an ID the agent holds returns the record without rescanning; for an ID it has released, the call rescans as for a new one. Two or more new devices, or none within the bound, fail the call and record nothing.
- **What the guest does with the device is nothing.** It does not mount, format or partition it, and its device node is left for the entrypoint. This stops at what #2 §6 item 9 leaves open and does not decide it.
- **`ReleaseDisk` refuses while the device is claimed, and otherwise removes it holding the claim.** The agent opens `/dev/<name>` read-only with `O_EXCL`. If the kernel answers `EBUSY` the call fails as a failed precondition with a `DiskInUse` status detail naming the disk, and any other failure of the open fails the call. Otherwise, with the descriptor still open, it writes `1` to `/sys/bus/pci/devices/<address>/remove`, waits up to a bound for the block device to be gone, then closes the descriptor and marks the record released. The record stays: a repeat for a released ID succeeds without a removal, so a release the daemon could not follow with the Driver's detach is repeated safely. `NOT_FOUND` means the agent never found the ID.
- **The daemon orders the calls around the Driver's.** Attach on a Running Machine records the holder, calls the Driver, then `FindDisk`; a failure of the guest step detaches again through the Driver and releases the holder, so the state after failure is not attached. Detach calls `ReleaseDisk`, then the Driver; a refusal is a precondition failure and the disk stays attached. A guest that does not answer or fails fails the Detach closed: the disk stays attached, and a client who needs it gone stops the Machine, after which detach involves no guest. A Driver detach that fails after a release asks the guest to find the disk again, best effort, so the guest and the VMM agree; if that fails too, or the daemon dies between the two, the same Detach called again has the guest release a second time without effect and tries the Driver again, and converges.
- **A Paused Machine's attach and detach are refused as a precondition failure.** A paused guest cannot answer, and an attach the guest never sees, or a detach it never released, is the case this decision exists to prevent. A Machine that is Created, Stopped or Faulted has no guest: attach and detach change the boot description and the record only, as before.

## Consequences

- A running guest sees an attached disk without a reboot, and a disk the guest has mounted is not unplugged from under it.
- The identification is by appearance, so it is sound only while the calls for a Machine are serialised and the kernel shows one new device per attach. A device that appears for another reason during the bound would be taken for the disk.
- The in-use check is the kernel's claim, held through the removal, so a mount cannot slip in between the check and the removal, and a filesystem of any kind, swap and a device-mapper table all stop a release. It sees claims and not opens: a process that holds the raw device open without claiming it (reading it, or writing it with `dd`) does not stop a release, and nor did the mount scan. The check needs the device node under `/dev`, which the agent mounts as devtmpfs; a missing node fails the release and does not refuse it.
- A release is repeatable, and the agent keeps one record per disk ID for the life of the guest.
- A disk the Machine booted with has no recorded ID, so it is not detachable while the Machine is Running; stopping the Machine first is the way. Whether the agent should find boot-given non-root disks, and by what, is left open.
- The Guest Agent's contract grows by two calls and one detail, the daemon's generated Go client changes with it, and the Guest port gains two operations every implementation of it serves. The Machine lifecycle now depends on the guest for a hot attach and detach, where it depended on the Driver alone.
- The kernel conditions gain `CONFIG_PCI`, `CONFIG_VIRTIO_PCI` and the sysfs `rescan` and `remove` files, and the VMM must run with `--enable-pci`.

## Signs the decision was wrong

- A real guest sees two new devices for one attach, or the new device is not the attached disk, in the real test or on a node.
- Firecracker, or a later VMM Driver, begins delivering a hotplug notification and the kernel's own handling replaces the rescan.
- Detaches fail often because the guest does not answer, and clients stop Machines to detach: the fail-closed rule is too strict.
- Item 9 is decided with an identity for a non-root disk (a UUID or a name), which would let the agent identify the device by it and drop the appearance rule.
