# ADR 0031: The Snapshot Store publishes a snapshot's state files owned by mvmd and read-only

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#216, found by the `e2e` job of garamsh/mvmd#214 (CI run 42779652144): a snapshot taken by one Machine did not restore into another Machine under Cloud Hypervisor. The restored VMM exited with `Error opening VM config snapshot file "/snapshot/config.json": Permission denied (os error 13)`. The state files in the store were `-rw------- 100000 100000`, owned by the origin Machine's user, and the restored Machine's VMM runs as another user of the node's range ([ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md) §Its own user).

What happens today, in `daemon/` at commit ce8a7ee085761858c1704c498f237e2afdada4f5:

- `Put` of the Snapshot Store takes the Driver's state files in by `os.Rename` and keeps the owner and mode the VMM gave them.
- The restoring Machine's VMM reaches a snapshot's files only through the root the supervisor builds, where they are bound read-only at fixed paths ([ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md)). A bind does not change the owner or the mode the file has on the host, so the VMM's user opens it only if the file's mode grants that user.
- What a VMM writes is its release's choice, not the store's. Cloud Hypervisor creates each state file with `create_new` at mode 0600 (`driver.md`). Firecracker v1.17.0 opens its state file and its memory file with Rust's `OpenOptions` and no mode (`src/vmm/src/persist.rs` `snapshot_state_to_file`, `src/vmm/src/vstate/vm.rs` `snapshot_memory_to_file`, at tag v1.17.0), so the kernel gives them 0666 less the umask, 0644 at the usual umask 022. Firecracker's restore across two users passed for that reason, which no rule of mvmd guarantees.
- A published snapshot is never written again, and many Machines may restore from one snapshot at once ([ADR 0016](./0016-a-restored-guest-reseeds-from-host-entropy-and-takes-its-time-from-the-host.md)'s clones). The store's directories are 0700 and mvmd's, so no other host user reaches the files. This is the reasoning `image.md` §Who may read the image gives for a published image's blob.

Candidates weighed:

- **At Put, before the publishing rename, the store gives each state file to mvmd and sets mode 0444** — chosen.
- **Give the files to the restoring Machine's user at restore.** Two concurrent restores from one snapshot need two owners, and the store's files would become a VMM user's.
- **Copy the state files into each restored Machine's run directory.** That is the guest's memory, gigabytes per restore, for no gain over a read-only bind.
- **A chmod in each Driver.** It puts a guarantee of the store in every Driver, and a mode of 0644 widens it to every host user for no reason the bind needs.

## Decision

At `Put`, before the publishing rename, the Snapshot Store makes each state file owned by mvmd's own user and group, mode 0444. The root disk copy is the restored Machine's own copy and stays as the store made it, 0600. The rule is the store's, so it holds whatever mode and owner a VMM release writes.

- **When the change fails**, the Put fails like the store's other steps: nothing is published, and what was built is removed.
- **A failed Put returns the state files with the owner and mode the Driver left**, since a Driver writes its files again over what a failed attempt left, as the user that wrote them, and could not open a file that is mvmd's and 0444.

## Consequences

- After Put, the origin Machine's user no longer owns the snapshot's files. Before, it could change them if it could reach them.
- Every user of the node's range can read a published snapshot's state files by the mode, and reaches them only where its root binds them (ADR 0019); no other host user reaches the store's directories.
- Giving a file to another user needs root, as `Lend` of [ADR 0024](./0024-the-converters-user-holds-the-output-file-only-while-its-vmm-runs.md) does. mvmd runs as root on a node, and a Put as another user fails at the step.
- A snapshot published before this rule keeps the owner and mode it has; the rule applies to Puts made after it.
- The Driver contract is unchanged, and the Drivers do not change.

## Signs the decision was wrong

- A VMM needs to write a snapshot's state file in place, so a published snapshot is not read-only after all.
- A node's snapshots directory has to be shared with another host user for a reason no bind covers, which makes world-readable state files reachable.
- A Put fails at the step on a host where mvmd holds the privilege to give files away, so the call is not reliable.
