# ADR 0009: A Machine's root disk is a sparse raw file with one holder

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#16 and decided in review of the pull request that adds this ADR.

The guest boot order overlays a writable disk on the read-only EROFS (`guest.md`, from garamsh/discussions#2 §5, discussioncomment-19288321). Create Machine takes a root disk size (`runtime-api.md` §Machine lifecycle). The host never mounts a guest disk, because a host kernel would interpret an untrusted filesystem and a second writer would be possible (#2 §2, discussioncomment-19288318). A Volume has at most one writer at a time (#2 §4 invariant 2, discussioncomment-19288320), and mvmd's part of it is one writer per disk on its node (`runtime.md` §Invariants). #2 §2 names dm-thin for sharing base disk blocks. Volume classes, durability and movement between nodes stay open (#2 §6 item 9); this ADR decides node-local disks only.

What the first two VMMs take as a disk:

- **Firecracker v1.17.0.** A drive is `path_on_host` with `is_read_only` (`src/firecracker/swagger/firecracker.yaml`, `Drive`); it is opened with write access unless read-only (`src/vmm/src/devices/virtio/block/virtio/device.rs`, `open_file`). The virtio-blk device ID it reports is built from the file's `st_dev`, `st_rdev` and `st_ino` (same file, `build_device_id`), so it cannot be chosen.
- **Cloud Hypervisor v54.0.** A disk is `path=` with `readonly=`, `image_type=<raw,qcow2,vhd,vhdx,vmdk>` and `serial=` among its parameters (`vmm/src/config.rs`, `DiskConfig::SYNTAX`). It checks the specified type against the file's contents (`block/src/factory.rs`, `open_disk`) and takes an advisory OFD lock on each disk, a read lock for a read-only disk and a write lock otherwise; the lock "does not enforce coordination across multiple hosts", and "a non-cooperating process can still open and write to a locked file" (`docs/disk_locking.md`; `virtio-devices/src/block.rs`, `try_lock_image`).

The overlay's upper filesystem "must support the creation of trusted.* and/or user.* extended attributes, and must provide valid d_type in readdir responses" (Linux v6.12, `Documentation/filesystems/overlayfs.rst`). `mke2fs` creates an ext4 filesystem "usually in a disk partition (or file)" and sets its UUID with `-U` (e2fsprogs v1.47.4, `misc/mke2fs.8.in`).

Candidates weighed:

- **A sparse raw file per root disk, formatted on the host before first use** — chosen.
- **A dm-thin volume per Machine.** Sharing base blocks is what dm-thin offers, and here the base is the shared EROFS, a separate read-only file, so a root disk starts empty and has no blocks to share. A thin pool needs a block device set aside on each node and its metadata kept healthy, and every root disk becomes a device to track across restarts. Snapshots of a disk are what would earn it.
- **qcow2.** Firecracker's `Drive` has no image-format parameter (`firecracker.yaml`, above), so a Firecracker disk is the file's bytes.
- **A block device or logical volume per Machine.** Requires node storage to be provisioned for mvmd before any Machine is created, where a file needs a directory.
- **A preallocated raw file.** Removes the risk of the host filesystem filling while guests write, at the cost of reserving every Machine's full size on disk; idle Machines and their count are where #2 §2 places cost.
- **Formatting by the Guest Agent at first boot.** The agent would need a formatter and a rule for "blank" that never matches a damaged filesystem, and a damaged disk would be reformatted rather than fail to mount.

## Decision

A Machine's root disk is a sparse raw file in the Machine's state directory, created by Create Machine at the root disk size and formatted ext4 on the host with a UUID mvmd chose, before any VMM opens it. It is the overlay's upper layer in the guest, and it is deleted by Delete Machine.

- **Creation.** Create Machine creates the file at the requested size without allocating it, formats it, and `fsync`s it and its directory before recording the Machine. A failure removes the file, and no Machine exists, as Create Machine's failure state reads.
- **Formatting is the host's only contact with its content.** The file is fresh, so formatting interprets nothing a guest wrote; after it, the host reads nothing from the disk.
- **Raw everywhere.** Every disk is handed to a Driver as a raw file, and a Driver whose VMM takes a format names raw, so nothing a guest writes can select a format.
- **One holder per disk.** A disk on this node has at most one holder, the Machine it is attached to. A root disk is held by its Machine for its whole life. The rule is the inside's, so it is tested with no VMM attached ([ADR 0002](./0002-mvmd-is-structured-as-a-hexagonal-application.md)): the holder is recorded durably before a Driver is asked to attach, and an attach naming a disk another Machine holds is precondition failed (`runtime-api.md` §Disks and resources). The Disk port creates, formats and deletes the files.
- **Release waits for the VMM.** A holder is released, by detach, delete or a stop that ends the VMM, only once the holder's VMM process has ended, so no second VMM opens a disk the first still has open.
- **Deletion.** Delete Machine removes the root disk file after the holder's VMM has ended; the file is not kept.
- **What stays open.** How a disk other than a root disk comes to exist on the node, its classes, durability and movement between nodes (#2 §6 item 9).

## Consequences

- Both Drivers take the root disk as it is, and the backing needs only a directory on the host filesystem.
- A root disk uses the space its guest has written, not its size, so the sum of root disk sizes on a node can exceed its free space. When the host filesystem fills, guest writes fail with I/O errors; nothing in this ADR reserves space.
- Cloud Hypervisor's own lock is a second, advisory guard under the CH Driver; under Firecracker, mvmd's record is the only one. Neither holds across nodes.
- The host needs `mke2fs`, and Create Machine pays a format.
- The guest kernel needs ext4 and overlayfs built in (`guest.md`, kernel conditions).
- Copying or snapshotting a disk has no support beyond copying the file; disk-and-memory snapshot pairs (#2 §2) are a decision of the snapshot store.

## Signs the decision was wrong

- Nodes run out of space under guest writes often enough that reservation is needed.
- Template snapshots (stage v2) or disk checkpoints need cheap disk clones that a raw file cannot give.
- A Machine finds its root disk written by another process, so the holder record did not keep a second writer out.
- Formatting at Create Machine is a large part of its latency.
