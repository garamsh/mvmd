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

- VMs survive an mvmd restart, until the recorded fencing expiry. The VMM runs as a separate process under its supervisor (§Fencing). Before serving any call, mvmd reattaches to each supervisor through its socket in the Machine's state directory, then to the VMMs through the Driver's `List` (#2 §5 design principle 4; [driver.md](./driver.md)).
- mvmd keeps its own state durably on local storage and continues after a restart (#2 §3 stability structure, the node as the reference for actual state).
- Running Machines keep running while the control plane or the client is down, until the fencing deadline passes, whether or not the mvmd process is running (#2 §3 stability structure, static stability).
- Lease, epochs, cells and the global layer belong to gaon.

## Fencing

If gaon's Node Agent dies while mvmd keeps VMs running, a rescheduled Machine could run on two nodes (#2 §4 invariant 1). mvmd therefore holds a deadline its client renews; when the deadline is not renewed in time, mvmd stops every Machine under it. The client sets the length of the deadline; the mechanism is mvmd's (#2 decision-change reply). How the client sets and renews it, and what holds across an mvmd restart, is in [runtime-api.md](./runtime-api.md) §Fencing deadline. The expiry is enforced whether or not the mvmd process is running: a VM runs at most δ past a recorded expiry, where δ is the time the enforcing process takes to end the VMM. The guarantee covers failures of mvmd, of the enforcing process and of the host, not a privileged host process acting on the enforcing process, and the client's margin covers δ ([runtime-api.md](./runtime-api.md) §Fencing deadline).

How it is enforced ([ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)):

- **A supervisor per Machine.** Every Machine whose VMM process exists runs under its own `mvmd-supervisor` process. The supervisor is the init process of a PID namespace and starts the VMM inside it, so the kernel kills the VMM and everything it started when the supervisor ends, however it ends.
- **No VMM without an expiry.** mvmd gives the supervisor the current expiry as part of its start, and the supervisor refuses to start the VMM without one or with one that has passed. Each start and each restore is a new supervisor.
- **A converter's VMM is supervised too**, with the end of the conversion time limit as its expiry in place of the fencing deadline, since it runs no workload ([ADR 0005](./adr/0005-untrusted-images-are-converted-in-a-converter-microvm.md)).
- **The supervisor counts the expiry.** It holds the expiry as an absolute `CLOCK_BOOTTIME` value, in mvmd's time namespace, and ends its namespace when the clock reaches it. It runs under a real-time scheduling policy that its VMM does not inherit, with its memory locked. It depends on nothing of mvmd's while it counts.
- **mvmd counts it too.** While running, mvmd kills every supervisor through its pidfd at the expiry.
- **Renewal reaches every supervisor before it is acknowledged.** An extension is recorded and then passed on; a shortening is passed on and then recorded, and a supervisor that does not take a shortening is killed first. No supervisor ever holds an expiry later than the recorded one.
- **Reboot.** The record carries the host's `boot_id`; a different one at start means no supervisor or VM survived, and the deadline is not armed.
- **The supervisor runs outside mvmd's cgroup**, so stopping mvmd under a service manager does not stop it.
- **Bound.** In each failure case the ADR lists, mvmd dying while it starts a Machine among them, a VM runs past the expiry for at most δ, the time the supervisor takes to wake and end its namespace. δ is not yet measured.

## Invariants

mvmd provides the mechanism for three of the invariants of #2 §4; gaon's Node Agent enforces invariants 4 and 5 (issue garamsh/mvmd#1, Context). Invariant 3, who writes spec, binding and status, concerns gaon's resource API and is not mvmd's.

| Invariant (#2 §4) | mvmd's part |
|---|---|
| 1. A Machine runs on at most one node at a time | The fencing deadline above |
| 2. A Volume has at most one writer at a time | One holder per disk on its node, recorded durably by the inside before a Driver attaches it and released only once the holder's VMM process has ended ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)) |
| 6. A Machine does not start until its network policy is applied | Fail-closed start: the Machine does not start until the policy is applied |

## Rationale

- The scope and the boundary: [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md).
- The enforcement mechanism, the candidates rejected and the bound in each failure case: [ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md).
- The disk backing and how the one-writer rule is enforced on the node: [ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md).
- Why the boundary sits at a runtime API and not at gaon's resource API: the original requirement of swappable components behind interfaces, and a one-way dependency (#2 §3 principle 3).

## Open questions

These are #2 §6 questions inside mvmd's scope, as they apply to the runtime. They bind nothing.

- **Preservation default (#2 §6 item 8):** whether a Machine keeps its disk only or its memory as well by default, and whether a snapshot is a public resource of the runtime API. #2 §2 offers disk-only as a proposal, not a decision.
- **Volume classes (item 9):** classes, durability (replication, backup) and movement between nodes, as far as the runtime stores or moves a volume. A Machine's root disk is decided, a node-local sparse raw file ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)); any other disk is not.
- **Network enforcement (item 10):** the technology that enforces external traffic policy on the node (nftables, eBPF and others are named in #2). The fail-closed start rule above holds whichever is chosen.
- **Density and restore numbers (item 13):** the fencing deadline's default, restore time and the latency right after a restore, and idle and active memory per node. None is measured; a value is set only after the single-node prototype of #2 §6 measures it.
- **The time a supervisor takes to end its namespace at the expiry**, under load and after a suspend; measured with the prototype numbers above.
- **How many earlier releases' supervisors mvmd must speak to**, since a supervisor outlives an mvmd upgrade until its Machine is next started.
