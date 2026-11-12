# ADR 0050: the guest agent mounts an attached disk's ext4 at a path and grows it online

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The agent found and released a non-root disk and did nothing else with it, because what such a disk is for was open (#2 §6 item 9). A client now needs a disk to appear at a path in the Machine, read-only when it was attached so, and to keep working when the disk is made larger. Only the agent can mount in the guest, and only the host can format, since a host that reads a guest's filesystem interprets untrusted bytes and the agent has no tool to format with. A disk named when the Machine is created is found before `Boot`, when the booted root does not exist, so a mount cannot be made by the `FindDisk` that names it. The path rules the file API and `RunConfig.files` already apply are the agent's one statement of where a client may reach.

## Decision

A disk is ext4 or has no filesystem, and the host formats it. `FindDisk` gains `mount_path` and `read_only`; the agent mounts the ext4 at the path in the booted root, creating the directory (0755, root), with `nodev` and `nosuid`, read-only when the attachment or the device is, and an empty path keeps today's behaviour. The path obeys the existing rules (absolute, clean, not `/`, not under `/proc`, `/sys`, `/dev` or `/run/mvmd`) through the same functions, and is resolved within the booted root before the mount. A disk found before `Boot` is recorded with its mount and mounted at a new step, `mount the disks`, after `place the files`; one found after is mounted by the call. `ReleaseDisk` unmounts what the agent mounted first, and a busy mount fails it as `DiskInUse`, naming the path. `GrowDisk {disk_id}` reads the device's size again and grows a mounted ext4 online with `EXT4_IOC_RESIZE_FS`, answering the size; a disk with no mount only has its size read. All of it is additive to `v1alpha1`.

## Consequences

A client reaches a disk's files at a path with no entrypoint of its own to mount it, and a restored guest keeps its mounts with the rest of its memory. The boot has an eleventh step and `BootFailed` an eleventh value, which a client built before it reads as an unknown enum value. A second disk for a held path, and a held ID repeated with another mount, are refused as a failed precondition and change nothing. A path where a file is placed is a path that is not a directory, so a mount there fails the boot at the new step. An online grow works only on a filesystem the kernel grows online: a read-only mount fails the call with the kernel's error, and a filesystem that is not ext4 is never mounted. Formatting a disk, and any API that names a path to a client, are not here. The grow and the unmount are shown only by tests that run as root on a loop device, so a developer without passwordless sudo does not see them run.

## Signs the decision was wrong

Clients attach disks with a filesystem other than ext4, or ask for the agent to format one. A mount is needed before the files are placed, or at an option other than `nodev,nosuid` and read-only. A disk's size shrinks or its filesystem needs an offline grow. The guest kernel the Driver boots lacks `EXT4_IOC_RESIZE_FS`.
