# ADR 0038: A disk file belongs to the Machine that holds it

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#288, found by the e2e test of Attach disk (garamsh/mvmd#271). A Machine's VMM runs as the Machine's own user ([ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md)), and a root disk is made owned by that user ([ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)). A disk made by Create disk is owned by mvmd's own user ([ADR 0036](./0036-the-runtime-api-creates-lists-and-deletes-a-node-local-disk-through-the-disk-adapter.md), which left open how a VMM's user comes to open it), so attaching it fails on both Drivers: Firecracker answers `Permission denied (os error 13)` to `PUT /drives`, and Cloud Hypervisor cannot open the disk path (CI run 42921593047 of garamsh/mvmd#287).

The forces:

- The change belongs where a disk takes a holder, which is the inside's rule ([ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)), so it holds for any Driver.
- A disk held by no Machine has no VMM user to belong to.

Candidates weighed:

- **The machine service gives the file to its holder and takes it back on release, through the Disk port** — chosen.
- **A Driver or the supervisor makes the file readable to the VMM's user.** Each Driver would carry the rule, and the supervisor would change the owner of a file the Disk adapter alone manages ([ADR 0021](./0021-a-root-disk-is-a-file-in-the-disks-directory-of-mvmds-state-directory.md)).
- **Create disk takes the Machine's user as an argument.** A disk is made before any Machine holds it and outlives each holder, so the user it was made for would be wrong for the next holder.
- **Make the file world-readable and writable.** Any host user could write a disk another Machine holds.

## Decision

1. **The Disk port changes a disk file's owner**: it gives the file to a Machine's user, or takes it back to mvmd's own user. A disk the node does not hold is the port's absent error.
2. **A Machine is given the file when it takes the holder slot, before any Driver is asked to open it.** Attach disk, hot or cold, and Create Machine for each disk it names give the file after the slot is taken. A restart gives each disk a recovered record lists to its Machine again.
3. **The file is taken back when the slot is released**: Detach disk, an attach that fails after the file was given, a Create Machine that fails after it, Delete Machine, and the restart's removal of a disk its VMM lacks. A disk the node no longer holds has nothing to take back.
4. **A failure to give fails the call and releases the slot**, with every file given by the call taken back. A failure to take back does not keep the slot: Detach disk and the restart report it with the slot freed, and Delete Machine fails before it removes anything, so a retry takes the file back.
5. **A Stop leaves the file with its holder**, since a stopped Machine still holds its disks.

## Consequences

- A disk made by Create disk attaches on both Drivers, and the e2e test of garamsh/mvmd#271 needs nothing written into the state directory.
- A free disk is owned by mvmd's user unless a death or a failed take-back left it with its last holder's user; the next attach gives it to the new holder and Delete disk removes it either way.
- Giving and taking a file are chown calls, so they need the privilege a root disk's creation already needs.
- Which user a Machine's VMM runs as is still decided by the UID allocator, so a disk is owned by the same user for the Machine's life.

## Signs the decision was wrong

- A Driver whose VMM opens a disk as a user other than the Machine's, so the owner is not what lets it in.
- A disk shared read-only by several Machines, which ADR 0009's one holder rules out today and which would need an owner no single Machine is.
- A node whose disks directory is on a filesystem that cannot change owners.
