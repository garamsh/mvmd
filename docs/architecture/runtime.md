# Runtime

What mvmd owns on a node, its boundary with gaon, and the invariants it provides.

Checked against garamsh/discussions#2 as read on 2026-11-08 (decision-change reply discussioncomment-19466961). Section links below are to the section comments of that discussion: §1 discussioncomment-19288316, §2 -19288318, §3 -19288319, §4 -19288320, §5 -19288321, §6 -19288322. The discussion's figures are estimates and nothing here states one as measured.

## Boundary with gaon

mvmd is a node-local microVM runtime, the kind of component containerd is to Kubernetes. It does not know gaon, imports nothing from gaon, and does not define gaon's resource API, Lease or epochs. gaon's Node Agent is its client and the dependency runs gaon to mvmd only (#2 decision-change reply; §3 principle 3).

| Concern | Owner | Source |
|---|---|---|
| Runtime API definition and versioning | mvmd | #2 decision-change reply |
| Machine lifecycle execution on the node | mvmd | #2 decision-change reply |
| Image pull, MachineImage conversion, node image cache | mvmd | #2 decision-change reply; §5 image section |
| Snapshot store | mvmd | #2 decision-change reply |
| VMM Drivers and the Guest Agent | mvmd | #2 decision-change reply |
| Local fencing deadline mechanism | mvmd | #2 decision-change reply |
| Watch, Lease renewal, epoch checks, status reports | gaon Node Agent | #2 decision-change reply |
| The decision to run, wake, suspend or stop a Machine | gaon Node Agent | #2 decision-change reply; §3 wake case |
| Desired state, binding and epochs | gaon | #2 §3 state ownership table |
| Cells, the global layer, Gateway and Cell Router | gaon | #2 §3 stability structure |

mvmd executes what its client asks and reports what happened. Wake signals from the Gateway and Cell Router go to gaon's Node Agent, which decides and calls mvmd.

## Runtime API scope

The runtime API models a Machine, not a Pod or a container, and is not CRI (#2 §1 conclusion; decision-change reply). Its scope:

- Machine create, start, stop, pause, resume, snapshot and restore.
- Disk attach and detach, and resource resize.
- Inspect, list and events.
- Exec, a file API and a console.

mvmd defines and versions it. Each operation states its idempotency and the state a Machine is left in after a failure (#2 §3 principle 5). The contract is published as `lib/runtimeapi/` and is a leaf module ([structure.md](./structure.md) §Repository layout). Its operations, calling rules and version are in [runtime-api.md](./runtime-api.md).

## State and stability

mvmd owns the actual VM state on its node and the snapshot and image caches; they are not exposed outside it (#2 §3 state ownership table). Desired state, binding and epochs are its client's.

- VMs survive an mvmd restart. The VMM runs as a separate process and mvmd reattaches through the Driver's `List` (#2 §5 design principle 4; [driver.md](./driver.md)).
- mvmd keeps its own state durably on local storage and continues after a restart (#2 §3 stability structure, the node as the reference for actual state).
- Running Machines keep running while the control plane or the client is down, until the fencing deadline passes (#2 §3 stability structure, static stability).
- Lease, epochs, cells and the global layer belong to gaon.

## Fencing

If gaon's Node Agent dies while mvmd keeps VMs running, a rescheduled Machine could run on two nodes (#2 §4 invariant 1). mvmd therefore holds a deadline its client renews; when the deadline is not renewed in time, mvmd stops every Machine under it. The client sets the length of the deadline; the mechanism is mvmd's (#2 decision-change reply). How the client sets and renews it, and what holds across an mvmd restart, is in [runtime-api.md](./runtime-api.md) §Fencing deadline.

## Invariants

mvmd provides the mechanism for three of the invariants of #2 §4; gaon's Node Agent enforces invariants 4 and 5 (issue garamsh/mvmd#1, Context). Invariant 3, who writes spec, binding and status, concerns gaon's resource API and is not mvmd's.

| Invariant (#2 §4) | mvmd's part |
|---|---|
| 1. A Machine runs on at most one node at a time | The fencing deadline above |
| 2. A Volume has at most one writer at a time | One writer per disk at a time on its node |
| 6. A Machine does not start until its network policy is applied | Fail-closed start: the Machine does not start until the policy is applied |

## Rationale

- The scope and the boundary: [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md).
- Why the boundary sits at a runtime API and not at gaon's resource API: the original requirement of swappable components behind interfaces, and a one-way dependency (#2 §3 principle 3).

## Open questions

These are #2 §6 questions inside mvmd's scope, as they apply to the runtime. They bind nothing.

- **Preservation default (#2 §6 item 8):** whether a Machine keeps its disk only or its memory as well by default, and whether a snapshot is a public resource of the runtime API. #2 §2 offers disk-only as a proposal, not a decision.
- **Volume classes (item 9):** classes, durability (replication, backup) and movement between nodes, as far as the runtime stores or moves a volume.
- **Network enforcement (item 10):** the technology that enforces external traffic policy on the node (nftables, eBPF and others are named in #2). The fail-closed start rule above holds whichever is chosen.
- **Density and restore numbers (item 13):** the fencing deadline's default, restore time and the latency right after a restore, and idle and active memory per node. None is measured; a value is set only after the single-node prototype of #2 §6 measures it.
- **Runtime API wire encoding and transport, and fencing while mvmd is down:** see [runtime-api.md](./runtime-api.md) §Open questions.
