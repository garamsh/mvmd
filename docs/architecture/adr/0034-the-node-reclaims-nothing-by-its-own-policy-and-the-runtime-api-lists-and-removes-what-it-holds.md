# ADR 0034: The node reclaims nothing by its own policy, and the runtime API lists and removes what it holds

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#234 and split into #238 (snapshots) and #234 (images). Space used by pulled images and by snapshots only grew: no runtime API call lists or removes either, and each snapshot holds a copy of a Machine's root disk. `image.md` named a garbage collection of the image cache as built, and no code removed a listed image.

The forces:

- mvmd is the node runtime and gaon's Node Agent is its client ([ADR 0001](./0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md)). Which image or snapshot is worth keeping depends on what the control plane intends, which the node does not know.
- A client that lost its records must be able to find out what the node holds.
- A removal must not break a Machine: not one that is being created or restored, and not a snapshot that could no longer be restored.

Candidates weighed for who decides what to reclaim:

- **The client decides; the node offers the calls to see and remove, and refuses a removal that would break a Machine** — chosen. It is the split Fly.io draws between its control plane and its node runtime.
- **The node reclaims by its own retention policy** (age, count or space). It needs a policy the node has no basis for, and it can remove what the client still means to restore.
- **A removal that never refuses**, leaving a later restore to fail. The node would then hold a snapshot or Machine that cannot work, and the client would learn of it at the worst moment.

## Decision

mvmd provides the mechanism; the control plane holds the policy. The node reclaims nothing by its own retention policy.

1. **A snapshot is a public resource of the runtime API.** `Snapshot` returns the identifier `Restore` takes, so it was public in practice; this records it. It settles only the "public resource" half of the Preservation default question in `runtime.md`; whether a Machine keeps its memory by default stays open.
2. **The runtime API gains a list and a removal for each of the two resources**: list snapshots and delete a snapshot, list images and remove an image. Each removal is idempotent: removing what is already gone succeeds. A list reports what the node holds, for reconciliation, and there is no separate inspect call.
3. **A removal refuses what is in use, with precondition failed.**
   - **A snapshot** a Snapshot call is writing or a Restore call is reading. The check is atomic with that call: the per-snapshot lock that Snapshot holds exclusively and Restore holds shared until it returns is taken exclusively without waiting by the delete. A snapshot whose Machine exists is not refused, since a snapshot is a copy.
   - **An image** a Machine record references in any state, or a snapshot the node holds references, since a restore locates the snapshot's image and the removal would leave the snapshot unrestorable. The client deletes the snapshot first. The check is atomic with Create and Restore by a per-digest lock they hold shared from locating the image until the Machine record is set, which the removal takes exclusively without waiting.
4. **Removing an image unlists it and reclaims its blobs at once**, not by the start-up sweep, which deletes every blob no listed manifest reaches and so cannot run beside a pull. The removal unlists the image with one index swap, deletes only the blobs it reached that no remaining listed manifest reaches, and shares one mutex with the pull's publish. Blobs orphaned by a crash after the swap are left to the next start's sweep.

Delivery: garamsh/mvmd#238 delivers the snapshot half (items 1 to 3, snapshot) and garamsh/mvmd#234 the image half (items 2 to 4, image). No note is planned on this ADR; `image.md` and `runtime-api.md` record what is built.

### The premise of the snapshot rule: a delete never writes to a file

A delete does not wait for a restored VMM to end, so it is safe only if removing the snapshot's files cannot change what a running restored VMM reads. Issue #238 put it as "the restored VMM does not read the state files once `RestoreSnapshot` returns". Checked against the two VMMs, that statement is true of one and not of the other; what holds for both is narrower, and it is the premise this decision rests on.

- **Cloud Hypervisor v54.0** restores memory in the default `MemoryRestoreMode::Copy` (`vmm/src/config.rs`, `vmm/src/memory_manager.rs`): the memory file is read into guest memory before the VM resumes. After `RestoreSnapshot` returns, it reads no state file.
- **Firecracker v1.17.0** maps the memory file with `libc::MAP_PRIVATE` (`src/vmm/src/vstate/memory.rs`, `snapshot_file`), so guest pages are read from the file lazily, on first touch, after the restore call has returned. The VMM does read the memory file after the call.
- **The Snapshot Store removes by unlinking.** The adapter renames the snapshot's directory into `staging/` and then calls `os.RemoveAll`; it never writes to, truncates or overwrites a state file. A file the VMM has mapped or open keeps its inode, and with it its content, until the last mapping or descriptor goes. The VMM sees the snapshot's files through bind mounts in its own mount namespace ([ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md)), which also hold the inode. The state files are mode 0444 and owned by mvmd ([ADR 0031](./0031-the-snapshot-store-publishes-a-snapshots-state-files-owned-by-mvmd-and-read-only.md)), so the VMM cannot write them either, and a private mapping cannot reach the file.

So the rule is "a delete only unlinks, and an unlinked file a VMM holds stays what it was", not "the VMM has finished reading". A Restore is refused against until it returns because until then the Driver is still taking the state files in.

## Consequences

- A client can reclaim snapshot space by the calls, and reconcile what the node holds by the list. The node never removes a snapshot or an image on its own, so a node whose client does not call grows.
- Space freed by a snapshot's delete comes back when a restored VMM that holds its files ends, not at the call, when Firecracker maps the memory file. A client sees the snapshot gone from the list at once.
- A delete of the snapshot a Machine was taken from, or of one a Machine was restored from, succeeds. A later restore of a deleted snapshot is not found.
- The two locks are the same kind: a per-key lock tried exclusively by the removal and held shared by the users. The snapshot's lock exists; the image's is added with #234. No generic keyed lock is built in advance of the second use.
- A snapshot being written is listed before the Snapshot call ends, and a client that deletes it by what it listed is refused.
- This rules out a removal that waits for the call to end, and a node policy that reclaims by age or space.

## Signs the decision was wrong

- A VMM that fails or reads wrong data after its snapshot was deleted, which would mean a state file is written to or replaced in place after a restore, or a Driver that reopens a state file by path after the call returns.
- A snapshot restored from after a delete refused it, or a delete that succeeded during a restore, which would show the lock check is not atomic with the call.
- A node that runs out of space because its client did not remove what it no longer needs, which would be the case for a retention policy on the node.
- A client that needs an inspect call or a filter on the list, because the list's fields do not carry what it decides by.
