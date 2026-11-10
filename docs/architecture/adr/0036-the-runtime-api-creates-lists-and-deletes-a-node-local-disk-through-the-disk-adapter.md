# ADR 0036: The runtime API creates, lists and deletes a node-local disk through the Disk adapter

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#270. Attach disk takes "a disk the node holds" (`runtime-api.md` §Disks and resources), and no operation makes one: [ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md) left open how a disk other than a root disk comes to exist on the node. [ADR 0021](./0021-a-root-disk-is-a-file-in-the-disks-directory-of-mvmds-state-directory.md) gave `disks/` to the Disk adapter alone. The e2e test of Attach disk (garamsh/mvmd#251) could only make its disk by writing into that directory itself, which review refused.

The forces:

- A client that lost its records must be able to find out what the node holds ([ADR 0034](./0034-the-node-reclaims-nothing-by-its-own-policy-and-the-runtime-api-lists-and-removes-what-it-holds.md)).
- The client chooses the identifier of every disk it creates and a creating call carries a request ID (`runtime-api.md` §Calling rules).
- A disk has at most one holder on the node, root disks and others alike (ADR 0009).
- Volume classes, durability and movement between nodes stay open (#2 §6 item 9); this decision settles what the runtime needs to hold a disk and nothing more.

Candidates weighed for how a non-root disk comes to exist:

- **Create, list and delete calls of the runtime API, served by the Disk adapter** — chosen. The adapter already creates, formats and removes the files of `disks/`, so the calls add no second writer to the directory.
- **A directory the operator provisions with disk files.** The node would hold files no mvmd call made, with no size or format mvmd chose and no record of which request made them, and `disks/` would have a second writer. A client cannot make a disk without a person on the node.
- **Disks created implicitly by Create Machine**, as its root disk is. A Machine would own every disk it was created with, so a disk could not outlive its Machine or move to another, and the client could not create a disk before the Machine that attaches it.

## Decision

1. **The runtime API gains Create disk, List disks and Delete disk.** Create disk takes a client-chosen identifier, a size in bytes and a request ID, and makes a sparse raw file in `disks/` through the Disk adapter, formatted ext4 as a root disk is. A repeat with the same request ID and size returns the original outcome; the same identifier under another request ID or size is a conflict. A failed create leaves no file. List disks reports every disk the node holds, root disks included, each with its size and holder, if any. Delete disk refuses a disk with a holder as precondition failed, and an absent disk succeeds.
2. **The request ID is kept with the file.** The adapter records it as an extended attribute of the disk file, set before the file is published, so a disk and the request that made it exist together or not at all. A root disk has none.
3. **A file is published without replacing another.** Create Machine and restore, which publish a root disk, and Create disk refuse a name a disk file already has, so a disk of the client's cannot be overwritten by a root disk of the same name.
4. **The holder is the inside's record.** The holder of a disk is read from the Machine records, as ADR 0009 has it, and Delete disk takes the disk's holder slot while it removes the file, so an attach cannot hold a disk being deleted.
5. **A Machine's root disk is not deleted by this call.** It has its Machine as holder for its life, so Delete disk refuses it as it refuses any held disk; Delete Machine removes it.
6. **The version stays `v1alpha2`.** Adding operations keeps the version (`runtime-api.md` §Scope and version).

## Consequences

- A client can make a disk, see it listed with its holder and delete it, and an attach e2e test needs nothing written into `disks/`.
- A disk outlives the Machine that held it: Delete Machine releases an attached disk and leaves its file.
- The disk file's filesystem must keep `user.*` extended attributes, which `disks/` on ext4, XFS and Btrfs do; a Create disk on one that does not fails.
- A disk the Create call makes is owned by mvmd's user. How a VMM's user comes to open a disk it is attached to is not decided here.
- This rules out disks provisioned by hand in `disks/`, and a disk that exists only as part of a Machine.

## Signs the decision was wrong

- A client that needs a disk class, replication or a move between nodes, which is the open question this ADR leaves.
- A state directory on a filesystem without `user.*` extended attributes, which would call for the record of a request to live in a file beside the disk.
- A client that needs to tell a root disk from another in the list, because it decides by that.
- A client that creates a disk and cannot attach it because its VMM's user cannot open the file.
