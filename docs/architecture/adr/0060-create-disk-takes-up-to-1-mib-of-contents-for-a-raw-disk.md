# ADR 0060: Create disk takes up to 1 MiB of contents for a raw disk

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A client that boots an OS-form Machine ([ADR 0058](./0058-an-os-form-machine-boots-a-node-named-kernel-and-initrd-with-a-command-line-and-disks-attached-at-boot-and-has-no-image-or-guest-agent.md)) has no Guest Agent to write a file for it, and a NoCloud seed is a small disk the guest reads by its label. Create disk made a sparse file, formatted or not ([ADR 0053](./0053-create-disk-takes-a-filesystem-and-an-attachment-takes-a-mount-path-the-guest-agent-is-asked-to-mount.md)), and the client had no way to put bytes on it. The seed is attached read-only with the existing flag, so nothing new is needed there. The decision was taken under garamsh/aurora#804 and recorded for garamsh/mvmd#662.

## Decision

`CreateDiskRequest` takes an optional `contents`: at most 1 MiB, accepted only with `FILESYSTEM_NONE`. mvmd writes the bytes at offset 0 and syncs them before the disk is published, so the call returns with the bytes durable and the disk and its bytes exist together or not at all.

- **Size.** The size is at least the contents' length; a size not set is the length rounded up to 512 bytes. A size below the length is invalid.
- **Refusals.** Contents over 1 MiB, contents with ext4 or with the filesystem unspecified, and contents with `from_snapshot_id` are `INVALID_ARGUMENT` naming `contents`. The unspecified filesystem is ext4 ([ADR 0053](./0053-create-disk-takes-a-filesystem-and-an-attachment-takes-a-mount-path-the-guest-agent-is-asked-to-mount.md)), so a client names `FILESYSTEM_NONE`. Empty contents are no contents.
- **Repeats.** The disk adapter records the SHA-256 of the contents as the extended attribute `user.mvmd.contents` before the file is published, as it does the request ID. A repeat with the same identifier, request ID, size, filesystem and contents returns the original outcome and writes nothing; the same identifier with other contents is a conflict, as other parameters are. A disk made with no contents has no such attribute.

This adds a field to `v1alpha2`, so the version stays. It changes what Create disk accepts, so it is a decision of its own; it amends no earlier ADR, and ADR 0036's and ADR 0053's text stands.

## Consequences

- The recorded digest is of what the call gave, not of what the file holds now: a guest that writes the disk does not change what a repeat is compared against.
- A disk with contents is an ordinary node-local disk afterwards: it is listed, attached, snapshotted, grown and deleted as any other.
- The bytes pass through the daemon's memory once, bounded by 1 MiB, and mvmd interprets none of them.
- A guest read of the bytes is not tested here; garamsh/mvmd#699 does it.

## Signs the decision was wrong

- Clients need a seed over 1 MiB, or several files on a formatted disk.
- Clients ask for contents with ext4, so mvmd would have to write a file system image.
- A repeat after the guest rewrote the disk is expected to compare against the file and not against the call.
