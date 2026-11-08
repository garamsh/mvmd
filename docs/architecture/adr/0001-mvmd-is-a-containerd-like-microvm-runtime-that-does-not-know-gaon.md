# ADR 0001: mvmd is a containerd-like microVM runtime that does not know gaon

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Decider: the project owner, @sauce-git, stated in the PM session of 2026-11-08. The discussions cited here are mutable pages; they were read on that date, and the replies are named by their comment ids.

- [garamsh/discussions#1](https://github.com/garamsh/discussions/discussions/1) took Kubernetes with Kata Containers as the execution base.
- [garamsh/discussions#2](https://github.com/garamsh/discussions/discussions/2) proposed orchestrating microVMs directly, without Kubernetes or CRI, behind interfaces that allow any component to be swapped as in Kubernetes. The owner adopted its direction in the [decision reply](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19466471), which supersedes #1's direction. #1's reasoning and objections stay on record there.
- The project is split in two: `gaon` orchestrates, `mvmd` is the node runtime ([naming reply](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19294946)).
- The decision reply first gave mvmd the Node Agent and everything below it, with gaon's resource API as the only contract, and the [supplementary reply](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19466927) gave mvmd the node data-plane API. The [decision-change reply](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19466961) replaces both on the same day. The reason given is the original requirement that components be swappable behind interfaces as in Kubernetes, and a one-way dependency in place of two repositories referencing each other (#2 §3 principle 3). The earlier boundary is recorded here as superseded and is not the current one.
- The [guest kernel reply](https://github.com/garamsh/discussions/discussions/2#discussioncomment-19466922) puts kernel build and management in a separate project.
- The figures in #2 (start, resume and memory numbers) are estimates and not measurements; this ADR relies on none of them.

## Decision

mvmd is a node-local microVM runtime daemon, the kind of component containerd is to Kubernetes. It does not know gaon, and it imports nothing from gaon. Its contract with its client is a VM-native runtime API that mvmd defines and versions.

- **mvmd owns:** the runtime API (Machine create, start, stop, pause, resume, snapshot and restore; disk attach and detach; resource resize; inspect, list and events; exec; file API; console), image pull, conversion to MachineImage and the node image cache, the snapshot store, the VMM Drivers, the Guest Agent, and a local fencing deadline.
- **gaon owns** the Node Agent (watch, Lease renewal, epoch checks, the OnDemand wake and suspend decision, status reports), which is mvmd's client. Wake signals from the Gateway and Cell Router go to gaon's Node Agent, which decides and calls mvmd. mvmd does not decide when to run, wake or stop a Machine; it executes what its client asks and reports what happened.
- **Fencing:** mvmd holds a deadline its client renews. When it is not renewed in time, mvmd stops every Machine under it. The client sets the length; the mechanism is mvmd's. Without it, a rescheduled Machine could run on two nodes after the Node Agent dies while mvmd keeps its VMs running (#2 §4 invariant 1).
- **Not CRI:** the runtime API models a Machine (pause, snapshot, hotplug), not a Pod or a container. #2 §1 found that CRI's limit was the Pod model's expressiveness, not that an interface existed.
- **VMM extensibility:** Firecracker and Cloud Hypervisor are the first Drivers, and the Driver contract must admit a VMM written later in-house (#2 §5 design principles 1 and 5, and the implementation-language table's row for an embedded VMM). The contract is stated in VMM-neutral terms with capability negotiation. Drivers start in-process in Go, with messages shaped for gRPC. An out-of-process Driver protocol, in the role containerd's shim API plays, is introduced when the first out-of-process VMM arrives.
- **No container runtime anywhere in mvmd**, development and CI included: no CRI, no Docker daemon, no containerd, runc or other OCI runtime, and no container as an isolation or execution unit. Hosts without KVM use nested virtualization or a local VM (#2 §5, isolation unit and runtime choice). The OCI image format remains only as the input and distribution format; mvmd pulls from registries and converts to MachineImage itself.
- **Guest kernel** build and management is its own project, created at the prototype stage when mvmd first needs to build a kernel itself. Until then mvmd holds no kernel configuration or patches.
- **Languages:** the daemon and its in-process Drivers in Go; the Guest Agent, and later the UFFD lazy-restore handler, in Rust (#2 §5, implementation language).
- **Dependency direction:** gaon depends on mvmd and mvmd on nothing of gaon. mvmd's documents and code do not define gaon's resource API, Lease or epochs.

The structure of the daemon is recorded in [ADR 0002](./0002-mvmd-is-structured-as-a-hexagonal-application.md).

## Consequences

- gaon can be replaced, or mvmd driven by another client, without mvmd changing; a different runtime could serve gaon if it offers the same API.
- Wake latency includes a call from gaon's Node Agent to mvmd, which a runtime that decided wakes itself would not pay.
- mvmd carries a safety mechanism, the fencing deadline, whose failure cannot be left to its client: a mvmd that fails to stop Machines at the deadline reintroduces the two-node case.
- The runtime API is a published, versioned contract owned by mvmd; every change to it is a change for gaon.
- mvmd needs its own image pipeline and snapshot store, and the cost of building what the Kubernetes ecosystem supplied (#2 §1) falls on this project.
- Rust code (the Guest Agent) arrives with no Rust stack convention in this repository; one is proposed separately.
- The Node Agent decisions in #2 §3 and §4 (OnDemand, epochs, Lease) are gaon's to record; this ADR records only that they are not mvmd's.

## Signs the decision was wrong

- mvmd needs gaon-specific knowledge to execute a request correctly: an epoch, a Lease or a resource kind of gaon's appears in the runtime API or in mvmd's state.
- A second client of the runtime API never appears and gaon's Node Agent reaches around the API, so the boundary only adds a hop.
- The Node Agent has to hold per-Machine runtime state (snapshots, images, the actual VM state) that mvmd was assigned, because mvmd cannot answer from its own.
- Machines run on two nodes after a Node Agent failure although the deadline is configured, so the fencing mechanism does not do what assigning it to mvmd assumed.
- Most runtime API changes are forced by a single client's needs, which means the API models that client rather than a Machine.
- The Driver contract needs a Firecracker or Cloud Hypervisor term to express an operation, or changes when the first out-of-process VMM arrives.
