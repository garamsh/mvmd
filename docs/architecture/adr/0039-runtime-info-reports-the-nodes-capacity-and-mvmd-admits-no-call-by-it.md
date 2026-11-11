# ADR 0039: Runtime info reports the node's capacity, and mvmd admits no call by it

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#279. Runtime info reported the API versions and the Drivers with their capabilities and CPU model, and nothing of what the node has. A client that places Machines (gaon's Node Agent) could not tell from mvmd whether a Create Machine, Create disk or Restore would fit, so it had to read the host behind mvmd's back. Which Machine runs where is the client's decision ([runtime.md](../runtime.md) §Boundary with gaon).

The forces:

- The line is the one [ADR 0034](./0034-the-node-reclaims-nothing-by-its-own-policy-and-the-runtime-api-lists-and-removes-what-it-holds.md) draws for reclaiming: the node provides the mechanism and the facts, and the client holds the policy.
- List machines already reports each Machine's resources, so the sum of what Machines are given is derivable by the client.
- The state directory's filesystem holds the disks, the snapshots and the image store, so its free space is what Create disk, Snapshot and Pull image consume.

Candidates weighed for who decides whether a call fits:

- **The node reports its totals and admits every call; the client decides** — chosen.
- **mvmd enforces capacity at admission**, refusing a Create Machine, Create disk or Restore that would exceed it. It needs an overcommit ratio and a host reserve, which are placement policy the node has no basis for, and it would refuse a call the client chose to make, for instance on a host whose memory is ballooned or whose disks are sparse.
- **Report the sum of what Machines are given**, beside the totals. A second figure could only disagree with the List the client already holds, and it would be one more figure for the node to keep current.

## Decision

1. **Runtime info gains a capacity of four totals the node measures**: the logical CPUs mvmd may run on (the affinity set the kernel reports for mvmd, which its VMMs inherit), the host's total memory in bytes (`MemTotal` of `/proc/meminfo`), and for the filesystem of mvmd's state directory its total bytes and the bytes available to an unprivileged process (`statfs`).
2. **The figures are read through a Node capacity port**, with an adapter on the kernel and an in-memory substitute. The available bytes are read at each call and never cached.
3. **A capacity that cannot be read fails the call**; runtime info does not report zeros for it.
4. **mvmd reports the sum of what Machines are given nowhere, and admits no call by capacity.** Create Machine, Create disk and Restore are not refused for exceeding the figures. Overcommit and the host's own reserve are the client's policy. A host that really runs out of a resource fails the call as it does today.
5. **The version stays `v1alpha2`**: the capacity is an added optional field ([runtime-api.md](../runtime-api.md) §Scope and version).

## Consequences

- A client decides placement from runtime info and List machines alone, without reading the host.
- The state directory's filesystem is reported, not each of `disks/`, `snapshots/` and the image store; they share it, and a node that mounts one of them elsewhere reports only what its state directory's filesystem has.
- The CPU figure is the process's affinity: a node started with a narrower affinity reports the narrower set, which is what its VMMs get.
- The memory figure is the host's, not a cgroup limit the daemon runs under.
- A client that wants headroom subtracts it itself.
- This rules out an admission check in the Machine lifecycle by capacity.

## Signs the decision was wrong

- A client that cannot place safely without the node's own accounting, because a call it makes concurrently with another overshoots between its reading of List and the call, which would be the case for admission on the node.
- A node whose disks, snapshots or image store sit on a different filesystem from its state directory, so the reported space is not the space those consume.
- A node run under a memory or CPU limit other than the host's, whose reported totals are not what its Machines can use.
