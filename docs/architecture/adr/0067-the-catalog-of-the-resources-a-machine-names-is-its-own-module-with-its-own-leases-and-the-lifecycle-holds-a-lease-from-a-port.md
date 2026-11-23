# ADR 0067: the catalog of the resources a Machine names is its own module with its own leases, and the lifecycle holds a lease from a port

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

`machine.Service` offers about fifty calls, keeps about forty fields and six families of keyed locks, and the order between those locks existed only as comments on the fields (garamsh/mvmd#788). Three groups of its calls need no Machine's lock: those that list and remove the images, the boot sets and the disk snapshots the node holds. Each has a store, a lock and verbs of its own. Pull image was taken out of `machine/` for the same reason ([structure.md](../structure.md) §Rationale).

What binds them to the lifecycle is one rule: a resource a Machine record names is not removed. Create and restore hold a lease on the resource they are about to name from resolving it until the record is set, so a removal that cannot take the lock now is beside a call about to name the resource, and is refused and not made to wait.

## Decision

The resources a Machine record names and does not own are held by a catalog module of their own, `daemon/internal/catalog/`, with its own calls and its own locks. It takes them one family at a time, and this decision covers the cut and the lock order for all of them.

- **What leaves `machine.Service`.** The calls that list and remove a resource and need no Machine's lock: List images and Remove image, then Add boot set, List boot sets and Remove boot set, then List disk snapshots and Delete disk snapshot. The runtime API's driving adapter calls them on the catalog.
- **What stays.** Create, restore and Start, which name a resource and hold its lease; Snapshot disk and Create disk from snapshot, which need the disk's holder, a Machine's lock and a pause; Resize disk and Delete disk, which need the holder. A call that needs a Machine's lock is the lifecycle's.
- **The port.** `machine.Catalog`, declared in the lifecycle's package as the side that needs it, gives a lease on a resource that is released by the function it returns. The catalog implements it, and the composition site gives it to the lifecycle. A lifecycle given none holds no lease, as a node with nothing to remove needs none.
- **What the catalog reads.** The Machine records, through the State Store port, and the snapshots, through the Snapshot Store port. It is given no `machine.Service` and calls none, so no call of the catalog waits for a Machine's lock.
- **The lock order, where it is enforced.** A lease is the innermost lock: the catalog takes no Machine's lock and no other lock while it holds one of its own, and takes its own exclusive lock only by a try that refuses and never waits. A call of the lifecycle that holds a lease can therefore be waited for by no call of the catalog, and the order between the lifecycle's locks and the catalog's is not a thing a call can get wrong. The order between the lifecycle's own locks is unchanged: a Machine's lock, then a snapshot's, then a lease; a disk's lock, then a disk snapshot's lease, then a Machine's lock. The keyed lock both modules use is a leaf package, `daemon/internal/keyedlock/`.
- **Tests.** The moved calls are tested through the catalog's interface and the tests of the lifecycle's calls that take a lease go through both modules. The tests of the old calls are replaced and none stays beside them.
- **No change** in the runtime API or in what any call does.

## Consequences

- `machine.Service` loses the moved calls and the fields of the moved stores and locks, and each family leaves it in a change of its own.
- A removal reads every Machine record from the State Store, where it read the lifecycle's records in memory. A record that could not be read names no resource, as the lifecycle reads it.
- The lock order of the lifecycle's own locks remains a rule of the lifecycle's comments.

## Signs the decision was wrong

- A removal needs a fact only a Machine's lock can give, and the catalog must call the lifecycle.
- The State Store's records fall behind the lifecycle's in a way a removal can see.
