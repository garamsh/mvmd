# ADR 0046: A bound the node is configured with is reported and refused as precondition failed, while measured capacity stays the client's

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#374. A node's configuration bounds how many Machines it can hold: the VMM user range gives each Machine a UID ([runtime.md](../runtime.md) §VMM confinement), and the network prefix gives each Machine an address per served family ([runtime.md](../runtime.md) §Network). Exhausting the user range was refused as precondition failed with nothing taken. Exhausting the prefix was not: Create Machine released the disks and kept the UID, and ended as failed, the outcome for an attempt that ran and did not finish. A client cannot tell from failed whether to retry, and the UID stayed taken until the node restarted. Neither bound was reported, so a client placing Machines (gaon's Node Agent) learnt it only by reaching it.

[ADR 0039](./0039-runtime-info-reports-the-nodes-capacity-and-mvmd-admits-no-call-by-it.md) has mvmd report measured capacity and admit no call by it, because the overcommit and the host's own reserve that a refusal would need are placement policy. A configured bound is another kind of figure: the operator wrote it, mvmd cannot exceed it, and a call past it cannot succeed on this node however the client weighs it.

Candidates weighed:

- **Leave the outcome to the resource: failed for the prefix, precondition failed for the range.** The client cannot treat two exhaustions of the same kind alike.
- **Report the bound as capacity the client reads, and admit no call by it** (extending ADR 0039). A Create past it fails anyway, so mvmd would admit a call it knows cannot succeed.
- **Refuse past a configured bound as precondition failed, with nothing taken, and report the bound and the count** - chosen.

For the refusal itself, two designs: check the resources before anything is taken, or take them in order and release what was taken before the call returns. The first needs an address check ahead of the disks and the UID that the allocator, which owns the prefix, would then have to answer twice, and the answer could change between the check and the allocation. The second keeps the allocator the one authority and the refusal is made where the shortage is found. The second is chosen.

## Decision

1. **Exhausting the user range or the address prefix ends Create Machine and Restore as precondition failed**, with a reason that names the resource, and nothing taken: the UID, the disks and any address already taken are released before the call returns, and no failure is recorded under the request ID, so the same request succeeds once a slot is free.
2. **Runtime info's `NodeCapacity` gains `machine_limit` and `machines`**, both additive. `machine_limit` is the smaller of the count of the node's VMM user range and, for each family the node serves, the addresses of its prefix that a Machine can be given (the prefix less its network address, the IPv4 broadcast address, the gateway and any resolver inside it). `machines` is the count of Machine records the node holds, read at each call. Conversions draw from the same user range for the time they run, so a node converting an image can refuse a Create below `machine_limit`.
3. **ADR 0039 stands for what the node measures.** The CPUs, memory and state-directory figures are the client's to weigh and mvmd admits no call by them. A bound the configuration states is the exception, and only for the user range and the prefix.
4. **The version stays `v1alpha2`**: both fields are additions.

## Consequences

- A client sees a node is full from `machines` reaching `machine_limit`, or from precondition failed at Create Machine or Restore, and can retry the same request elsewhere or here once a Machine is deleted.
- A failed Create no longer leaves a UID taken when the prefix is exhausted.
- `machine_limit` is a ceiling, not a promise: a conversion in progress, or a Create that races another, can reach the refusal below it.
- The figure is computed from the configuration at start and cached; the count is read at each call.

## Signs the decision was wrong

- A client needs a bound that is not configured (for instance the host's file descriptors) reported the same way, so the line between a configured bound and a measured one was drawn in the wrong place.
- `machines` and List disagree for want of a Machine record that holds no UID, so the count should be of UIDs held.
- A node whose conversions are frequent refuses Creates below `machine_limit` often enough that clients cannot plan by it.
