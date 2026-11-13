# ADR 0058: The Guest Agent gives a fresh disk's root to the entrypoint's user

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The host formats a disk with `mkfs.ext4`, which leaves the root of the filesystem owned by 0:0 with mode 0755, and the Guest Agent mounts it at the path the attachment names ([ADR 0050](./0050-the-guest-agent-mounts-an-attached-disks-ext4-at-a-path-and-grows-it-online.md), [ADR 0053](./0053-create-disk-takes-a-filesystem-and-an-attachment-takes-a-mount-path-the-guest-agent-is-asked-to-mount.md)). An entrypoint that runs as another user ([ADR 0048](./0048-the-guest-agent-runs-the-entrypoint-as-a-named-user-and-places-files-from-the-boot-call-in-a-tmpfs-it-alone-mounts.md)) therefore cannot write in the disk it was given (#648). The client cannot fix it first: the disk is mounted when the entrypoint starts, and the entrypoint is the only process of the workload. The guest alone knows the identity, which `apply the user` resolves from the booted root's `/etc/passwd` and `/etc/group`, so the host cannot name it when it formats.

Taken and not taken: an owner on `DiskAttachment` would add a field to the runtime API and make the client resolve a name only the image knows; Docker's rule for an empty named volume, which copies the image's directory ownership onto it, has no counterpart here since the disk is not the image's; a recursive chown would rewrite a disk the workload has used.

## Decision

**The Guest Agent applies the entrypoint's user; there is no new field.** It sets the owner and group of the root of a filesystem it mounts to the uid and gid the entrypoint runs as, resolved as `apply the user` resolves them, once, on a filesystem still as `mkfs.ext4` left it: root directory 0:0, mode 0755, holding only `lost+found`. The mode stays 0755 and nothing under the root changes.

It is done for a read-write mount only. A disk mounted at the boot step `mount the disks` is owned in `apply the user`, after the identity is resolved and before the entrypoint starts; a disk attached to a running Machine is owned at its mount with the identity the boot resolved. A root (0:0) entrypoint changes nothing. A filesystem the workload has used is never changed, so a later mount, a restore (the mounts are in the guest's memory) and another Machine, which runs as another user or the same one, leave the owners as they are.

## Consequences

A non-root entrypoint writes in a disk mounted at a path with no step of the client's, and the runtime API is unchanged (`v1alpha2` stays). The test for a fresh filesystem is made on the mounted root, so it needs no state besides the filesystem. A disk first used by one user is that user's: another Machine that attaches it with another entrypoint user finds it no longer fresh and cannot write unless the first workload opened it. A disk the client populated before the first mount, even with one file, is never given away, so a client that wants a particular owner on such a disk sets it itself. The agent writes to the disk at boot, one `chown` for each such disk.

## Signs the decision was wrong

Clients attach one disk to Machines with different users and expect each to write. Clients populate a disk before its first mount and expect its root to follow the user. An image's user needs a mode other than 0755 on the root. A kernel or filesystem the agent mounts is found to leave a root different from what `mkfs.ext4` makes, so a fresh disk is not recognised.
