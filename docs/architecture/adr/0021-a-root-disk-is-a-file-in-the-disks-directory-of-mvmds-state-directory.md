# ADR 0021: A root disk is a file in the disks directory of mvmd's state directory

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#117, found while working on garamsh/mvmd#116, which wires the Disk adapter into the daemon. This ADR amends one place of [ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md) and leaves the rest of it as it is: the clause that puts a root disk "in the Machine's state directory". ADR 0009's body stays frozen, as ADR 0015's did when [ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md) amended it.

What the Machine's state directory is, in `daemon/` at commit 1cbb4f64e290b2cf6582d7e6ac3964187cde6798:

- `daemon/internal/supervision/supervision.go` creates it when a supervisor starts, and `Live` reads every subdirectory of the directory that holds them as a Machine's, looking for its supervisor's socket.
- `daemon/internal/statedir/` names the files the supervisor makes in it.

What a root disk is:

- It exists from Create Machine, before any start, so before the supervision adapter has made the directory.
- It outlives a stop, so a directory the supervision adapter makes at each start is not where it can live.
- A restore from a snapshot creates a new root disk as a copy of the snapshot's (`machine.Disk.CreateRootFrom`), for a Machine that has not started.

The directories of mvmd's state directory each have one adapter: `images/` the Image Store and `snapshots/` the Snapshot Store, and `conversions/` the supervision adapter that serves conversions.

Candidates weighed:

- **A `disks/` directory of mvmd's state directory, owned by the Disk adapter alone** — chosen.
- **The Machine's state directory**, as ADR 0009 says. Two adapters would write one directory, and the supervision adapter treats what it finds there as its own.
- **A directory per Machine under `disks/`.** A root disk is one file, and the Machine's other disks are not decided (ADR 0009, what stays open), so the directory would be a claim the code does not support.

## Decision

A root disk is a file in a `disks/` directory of mvmd's state directory, one file per Machine. The Disk adapter alone creates, reads and removes entries in it, and nothing else of mvmd writes there. Everything else ADR 0009 decides about a root disk stands: a sparse raw file, formatted ext4 on the host, one holder, deleted by Delete Machine.

A root disk made from a snapshot's is a sparse copy, made under a temporary name in `disks/` and published by rename, so a crash leaves the whole file or none.

## Consequences

- The supervision adapter and the Disk adapter share no directory, so neither reads the other's entries.
- A root disk is reached through the Disk port by its identifier, as it was; no other unit needs its location.
- The copy that snapshots and restores make of a disk file is one function, in `daemon/internal/sparsecopy/`, which the Snapshot Store adapter and the Disk adapter both call.
- ADR 0009 still reads "the Machine's state directory"; the architecture README's index line for it, `runtime.md` and `structure.md` state the present location.
- `disks/` is not under a Machine's state directory, so removing a Machine's state directory does not remove its root disk; Delete Machine removes the file through the Disk port, as ADR 0009 says.

## Signs the decision was wrong

- The Machine's other disks need a place and one file per Machine in `disks/` cannot hold them.
- A node's restart finds files in `disks/` that no Machine record names, often enough that the Disk adapter needs a sweep.
- A deployment needs a root disk on a file system other than the one holding mvmd's state directory, so that `disks/` cannot be a plain directory of it.
