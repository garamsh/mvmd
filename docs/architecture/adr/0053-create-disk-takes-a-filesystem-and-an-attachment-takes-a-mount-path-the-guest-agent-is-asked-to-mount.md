# ADR 0053: Create disk takes a filesystem and an attachment takes a mount path the guest agent is asked to mount

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The guest agent mounts an attached disk's ext4 at a path ([ADR 0050](./0050-the-guest-agent-mounts-an-attached-disks-ext4-at-a-path-and-grows-it-online.md)), but the runtime API gave a client no way to name that path, and the disk adapter formatted every disk ext4 with no way to leave one raw. A client needs to say at Create disk whether the disk holds a filesystem, and at attach or Create Machine where it appears (#439). The host formats, because the agent has no tool to format with and a host that reads a guest's filesystem interprets untrusted bytes. A disk made before this ADR has no record of its filesystem, and every one of them was formatted ext4.

## Decision

`CreateDiskRequest` takes `filesystem`: `FILESYSTEM_EXT4`, `FILESYSTEM_NONE`, or unspecified, which is ext4 so a client built before it is unchanged. Any other value is invalid. The disk adapter runs `mkfs.ext4` for ext4 and leaves the file sparse and unformatted for none, and records the filesystem as the extended attribute `user.mvmd.filesystem` before the file is published, as it does the request ID; a file with no such attribute is ext4. `Disk` and List disks report it. A repeat of Create disk is the same only with the same filesystem, the unspecified value counting as ext4.

`DiskAttachment` takes `mount_path`, which Attach disk and Create Machine pass to the guest agent's `FindDisk` with `read_only`. An empty path keeps today's behaviour, no mount. A path must be absolute and clean, not `/`, and not under `/proc`, `/sys`, `/dev` or `/run/mvmd`, the rules `RunConfig.files` applies through the same function; a path on a disk with no filesystem, and two disks of one Machine at one path, are invalid. The Machine keeps the path with the attachment, and a snapshot records it per disk (`SnapshotDisk.mount_path`) and a restore takes it, since the restored guest's agent holds the mount record and a later `FindDisk` with another path would be refused. A snapshot written before this has none, and restores its disks with no mount path.

## Consequences

A client gets a formatted, mounted disk with no entrypoint of its own to mount it, on both Drivers, and a client that wants a raw device keeps one. The host needs `mkfs.ext4` as it did. A disk's filesystem is a property of the file and cannot be changed, so a client who wants another makes another disk. The boot or the attach fails, as ADR 0050 says, for a path the guest cannot mount at; the daemon refuses what it can tell beforehand and no more. Formatting any other filesystem, growing a disk through the API and a mount option other than the agent's own are not here.

## Signs the decision was wrong

Clients ask for a filesystem other than ext4, or for mount options. A disk file is copied between nodes and loses the attribute, so it reports ext4 for a raw device. Clients put the same disk at a different path for each Machine and find the snapshot's recorded path in the way. Clients create disks with the unspecified value and expect them raw.
