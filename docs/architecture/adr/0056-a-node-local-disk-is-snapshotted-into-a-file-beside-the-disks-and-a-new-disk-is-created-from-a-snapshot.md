# ADR 0056: A node-local disk is snapshotted into a file beside the disks, and a new disk is created from a snapshot

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A client that wants a point-in-time copy of a node-local disk, or a second disk that starts from the first one's data, had no call for it. A disk is one sparse raw file ([ADR 0036](./0036-the-runtime-api-creates-lists-and-deletes-a-node-local-disk-through-the-disk-adapter.md)), and a Machine snapshot already keeps its disks as sparse copies in a store beside the state directory's `disks/` ([ADR 0041](./0041-a-snapshot-records-its-disks-a-restore-takes-exactly-those-and-a-non-root-disk-reaches-the-guest-by-hot-plug.md)). What is open is where a disk snapshot lives, what a repeat means, what a Machine that holds the disk does, and how the new disk relates to the snapshot (#524).

## Decision

The calls are `SnapshotDisk {disk_id, snapshot_id, request_id}`, `ListDiskSnapshots` and `DeleteDiskSnapshot {snapshot_id}`, and `CreateDiskRequest` gains `from_snapshot_id`.

A snapshot is a file in `disk-snapshots/` of the state directory, beside `disks/` and on its file system. It is a `FICLONE` reflink of the disk's file where the file system supports one and a sparse copy where it does not, and the store makes it in a staging directory and publishes it by one rename, so a snapshot exists whole or not at all. `runtime.md` says the two directories share a file system, and the node checks it at start and refuses to start when it fails. A snapshot identifier obeys the rules of a disk identifier.

Repeats follow Create disk. The same snapshot identifier, request ID and disk is a success that makes nothing again; the same identifier under another request ID or disk is a conflict. `from_snapshot_id` joins Create disk's repeat key, so the same call is a success even after its snapshot was deleted, and another snapshot under the same disk identifier is a conflict. Delete disk snapshot of an absent snapshot is a success.

A disk no Machine holds, or one a Created, Stopped or Paused Machine holds, is copied as it is. A Running Machine's disk is copied with the Machine paused, and the Machine is returned to Running; if it cannot be, the Machine is Faulted, as Snapshot's failure row says. A Faulted holder, or any other state, is precondition failed. The snapshot of a Running Machine's disk is crash-consistent, and the documents say so.

A disk created from a snapshot is a copy of the snapshot's file and takes its filesystem: an unspecified or equal filesystem in the request is accepted, any other is invalid. A size below the snapshot's is invalid, and a larger size grows the new file. The guest grows the filesystem at the Machine's next Start ([ADR 0055](./0055-a-node-local-disk-grows-offline-or-online-on-a-driver-that-reports-disk-resize-and-the-guest-grows-the-filesystem.md)). An unknown snapshot is not found. The disk file records the snapshot it came from as an extended attribute, beside the request.

Delete disk snapshot is precondition failed while a Snapshot disk or Create disk call uses the snapshot. Deleting the source disk does not touch its snapshots, and deleting a snapshot does not touch a disk made from it.

## Consequences

A client keeps and branches a disk with no copy of its bytes where the file system has reflinks, and with a sparse copy elsewhere. A snapshot of a Running Machine's disk costs the Machine a pause for the copy, which a reflink makes short and a sparse copy makes as long as the disk's allocated bytes. The node refuses to start when `disks/` and `disk-snapshots/` are on different file systems, which a deployment that mounts them apart has to change.

## Signs the decision was wrong

Clients need a disk snapshot that is consistent for a filesystem, so that they flush in the guest before every call. The pause of a Running Machine under a sparse copy is longer than clients accept. Deployments keep the state directory on file systems that cannot be one. Clients need a snapshot to move between nodes.
