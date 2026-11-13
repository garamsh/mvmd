# ADR 0055: A node-local disk grows, offline or online on a Driver that reports disk-resize, and the guest grows the filesystem

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A disk's size was fixed at Create disk. A client whose data outgrew its disk had to make a larger disk and copy into it (#523). The guest agent already grows an ext4 filesystem to fill its device, repeatably, with `GrowDisk` ([ADR 0050](./0050-the-guest-agent-mounts-an-attached-disks-ext4-at-a-path-and-grows-it-online.md)), and the host does not touch a guest's filesystem ([ADR 0053](./0053-create-disk-takes-a-filesystem-and-an-attachment-takes-a-mount-path-the-guest-agent-is-asked-to-mount.md)). What is open is how the file and the VMM's drive grow, and which Drivers can do it while the guest runs.

The pinned VMMs differ. Cloud Hypervisor v54.0 has `PUT /api/v1/vm.resize-disk` with `{"id", "desired_size"}` (`docs/api.md`, `cloud-hypervisor.yaml`, and the release note "Live Disk Resizing Support for Raw Images"): the VMM itself sets the length of the raw file, which it must have open read-write, and tells the guest by a configuration change. The size must be a multiple of 512 bytes. Firecracker v1.17.0 has `PATCH /drives/{drive_id}` (`docs/api_requests/patch-block.md`), which is documented for a drive the guest has not mounted and does not change the host file; it is not a live growth of a disk the guest uses.

## Decision

`ResizeDisk {disk_id, size_bytes}` grows a disk to a larger size and answers an empty response. An unknown disk is not found, a size below the disk's is invalid, and an equal size succeeds and changes nothing. The growth depends on the holder:

- A disk no Machine holds, or one a Created or Stopped Machine holds, has its file grown by the disk adapter, with `ftruncate` on a descriptor opened without following a link. No VMM has it open, and its file belongs to mvmd's user, since a Machine's disks are given to its user at Start and back at Stop ([ADR 0038](./0038-a-disk-file-belongs-to-the-machine-that-holds-it.md)). The filesystem is grown by the guest at the Machine's next Start: after `Boot`, the lifecycle calls `GrowDisk` for each attachment that has a mount path and is not read-only. The call is stateless and repeatable, and a failure fails the start, so a Machine is not Running with a filesystem smaller than its device.
- A disk a Running Machine holds grows live only on a Driver that reports the capability `disk-resize`, `CAPABILITY_DISK_RESIZE` on the wire. The lifecycle asks the Driver to grow the drive, then asks the guest's `GrowDisk` until the device it sees has the new size, for at most the resize timeout, and the call returns after that. A disk attached read-only is not grown live, since the VMM opened it read-only.
- Any other holder state, a Paused or Faulted Machine, a Running Machine on a Driver without the capability, and a Machine's root disk in any state, is precondition failed. The root disk is held for its Machine's life and is sized by Create Machine; this decision does not change that.

A disk with no filesystem grows its device only. The call holds the disk for its duration, so that no attach, delete, Create Machine or Restore takes it meanwhile; List disks shows no holder for it.

Cloud Hypervisor reports `disk-resize` and sends `PUT /vm.resize-disk` with the device's name, refusing before the request a size that is zero or not a multiple of 512. Firecracker does not report it and its `ResizeDisk` answers unsupported, citing `patch-block.md`.

An equal size on a Running Machine's disk on a Driver with the capability still asks the Driver and the guest, so a call that failed after the VMM had grown the file is finished by the repeat.

## Consequences

A client grows a disk in place, offline on both Drivers and live on Cloud Hypervisor, and the guest's filesystem follows with no host access to it. A repeat of Create disk with the original size after a growth is a conflict, since the size differs. A restore refuses a disk whose size differs from the snapshot's record ([ADR 0041](./0041-a-snapshot-records-its-disks-a-restore-takes-exactly-those-and-a-non-root-disk-reaches-the-guest-by-hot-plug.md)), so a snapshot taken before a growth is not restorable after it. A disk does not shrink. Growing a root disk, a filesystem other than ext4 and a live growth on Firecracker are not here.

## Signs the decision was wrong

Clients grow disks of Machines they would rather not stop, on Firecracker. A guest takes longer than the resize timeout to see a grown device. A client needs a root disk to grow. The restore refusal makes clients delete snapshots they wanted after every growth. A VMM's live growth of a file fails for a reason the Driver's error does not show.
