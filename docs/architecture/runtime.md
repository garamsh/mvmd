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
- **The store is bbolt**, one database file, `mvmd.db`, in mvmd's state directory (the daemon's configuration names it). It holds the Machine records with their request IDs, and the fencing deadline record, a bucket each. A write returns once its transaction is committed and the file synced, which the fencing rule "acknowledged only after durably recorded" needs ([runtime-api.md](./runtime-api.md) §Fencing deadline). It is embedded and pure Go, and one process holds the file at a time.
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
- **mvmd confirms an end by the namespace.** After it kills a supervisor it waits until no process of the host is in the supervisor's PID namespace, read from `/proc/<pid>/ns/pid`. A process of another user is visible there only to a reader with ptrace access, so mvmd needs `CAP_SYS_PTRACE` as well as `CAP_SYS_ADMIN`.
- **Bound.** In each failure case the ADR lists, mvmd dying while it starts a Machine among them, a VM runs past the expiry for at most δ, the time the supervisor takes to wake and end its namespace. δ is not yet measured.

## Network

How a Machine reaches the network on its node ([ADR 0011](./adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md), [ADR 0012](./adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)). Cross-node networking, the Gateway and the Cell Router are gaon's (#2 §3, §4).

- **One routed tap per Machine.** mvmd creates it, with its link down, before the VMM starts and deletes it after the VMM has ended. No two Machines share an L2 segment; the host is each guest's only next hop.
- **Addressing, dual stack.** The node's configuration names an IPv4 prefix, an IPv6 prefix, or both. mvmd assigns each Machine one address per configured family, at Create Machine and at Restore, and releases them at Delete Machine. The gateways are an IPv4 address and `fe80::1`, the same on every tap. The Guest Agent configures the addresses statically; no router advertisement or DHCP runs. A restored Machine gets new addresses, which the agent applies after the restore.
- **Enforcement.** One nftables table mvmd owns. At each tap, frames not from the Machine's MAC and addresses are dropped, ARP and the ICMPv6 neighbour solicitations and advertisements it needs are admitted, and router advertisements, redirects and router solicitations from a guest are dropped. The inet chains accept only what the Machine's policy allows, plus replies and their ICMP errors; everything else is dropped. Traffic leaving the node is masqueraded to the node's address of its family, IPv6 included.
- **Changed only by transactions**, which take effect whole or not at all (ADR 0012, at the Linux and nftables versions it names), and the table outlives an mvmd restart, which replaces it from mvmd's durable state before serving any call.
- **No IP traffic from a VMM's own sockets.** The table's output chain drops every packet whose socket belongs to a UID in the node's VMM range, which covers a VMM that shares the host's network namespace ([ADR 0015](./adr/0015-the-supervisor-confines-every-vmm-process-the-same-way.md)).

## VMM confinement

How the supervisor confines the VMM process it starts, Machine and converter alike, so that a guest escaping into its VMM reaches as little of the node as possible ([ADR 0015](./adr/0015-the-supervisor-confines-every-vmm-process-the-same-way.md), at the Linux, Firecracker and Cloud Hypervisor versions it names).

- **The supervisor confines, the same way for every VMM.** Its inputs are the Driver's description of the VMM process ([driver.md](./driver.md)); no step depends on which VMM runs, and none can be skipped. The VMM's own seccomp filter stays on beneath it. Firecracker's jailer is not used.
- **A user per Machine.** One UID and GID from the node's VMM range per Machine, allocated at Create Machine and Restore and released at Delete Machine, and one per conversion. It owns the Machine's tap, root disk and run directory, and no supplementary groups.
- **A mount namespace with nothing else in it.** An empty root holding the VMM executable, the boot description's files with their read-only flags, the host files and device nodes the Driver names, and the Machine's run directory, read-write. No `/proc` or `/sys`. The VMM's API and vsock sockets are in the run directory, where mvmd connects by path; a disk attached while it runs is hard-linked there, and a snapshot is written there and moved to the snapshot store.
- **A network namespace with only loopback**, for a converter and for a VMM whose Driver takes the tap by descriptor. A VMM that opens the tap by name, Firecracker v1.17.0, shares the host's, under the output rule in §Network.
- **No capabilities and `no_new_privs`.** The bounding and ambient sets are emptied before the change to the Machine's UID, so the VMM execs with none and cannot gain any.
- **Landlock ABI 6.** File access only beneath the mount namespace's paths, `ioctl` only on the named devices, no TCP bind or connect, and no abstract Unix socket or signal outside its own domain. mvmd refuses to start on a kernel without it.
- **A seccomp baseline** stacked under the VMM's own filters: no creating or entering namespaces (`unshare`, `setns`, `clone` with a namespace flag), and `clone3` answered `ENOSYS` so its flags are checked through `clone`.
- **A cgroup per VMM**, under a parent of mvmd's outside mvmd's cgroup, entered at creation: `memory.max` at guest memory plus the Driver's overhead, `pids.max` at the Driver's task count, `cpu.max` at the vCPU count plus the Driver's overhead. The supervisor is not in it; on a kernel with `CONFIG_RT_GROUP_SCHED` the supervisors are in the root cgroup.
- **Order, before `execve`:** open the descriptors to pass, enter the namespaces and build the root, drop capabilities and change user, `no_new_privs`, Landlock, the baseline filter. A failure at any step fails the start before the VMM runs.

## Invariants

mvmd provides the mechanism for three of the invariants of #2 §4; gaon's Node Agent enforces invariants 4 and 5 (issue garamsh/mvmd#1, Context). Invariant 3, who writes spec, binding and status, concerns gaon's resource API and is not mvmd's.

| Invariant (#2 §4) | mvmd's part |
|---|---|
| 1. A Machine runs on at most one node at a time | The fencing deadline above |
| 2. A Volume has at most one writer at a time | One holder per disk on its node, recorded durably by the inside before a Driver attaches it and released only once the holder's VMM process has ended ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)) |
| 6. A Machine does not start until its network policy is applied | Fail-closed start: the Machine's rules are committed in one nftables transaction before its tap's link comes up and before its VMM starts; a failed commit fails the start ([ADR 0012](./adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)) |

## Rationale

- The scope and the boundary: [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md).
- The enforcement mechanism, the candidates rejected and the bound in each failure case: [ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md).
- The disk backing and how the one-writer rule is enforced on the node: [ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md).
- The node network and its enforcement: [ADR 0011](./adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md), [ADR 0012](./adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md).
- Why the supervisor confines every VMM itself rather than each VMM's own mechanism, and what each VMM needs open: [ADR 0015](./adr/0015-the-supervisor-confines-every-vmm-process-the-same-way.md).
- Why the boundary sits at a runtime API and not at gaon's resource API: the original requirement of swappable components behind interfaces, and a one-way dependency (#2 §3 principle 3).

## Open questions

These are #2 §6 questions inside mvmd's scope, as they apply to the runtime. They bind nothing.

- **Preservation default (#2 §6 item 8):** whether a Machine keeps its disk only or its memory as well by default, and whether a snapshot is a public resource of the runtime API. #2 §2 offers disk-only as a proposal, not a decision.
- **Volume classes (item 9):** classes, durability (replication, backup) and movement between nodes, as far as the runtime stores or moves a volume. A Machine's root disk is decided, a node-local sparse raw file ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)); any other disk is not.
- **I/O limits per Machine.** A VMM's cgroup limits memory, tasks and CPU, not disk I/O, which needs per-device figures from the node's storage (§VMM confinement).
- **Density and restore numbers (item 13):** the fencing deadline's default, restore time and the latency right after a restore, and idle and active memory per node. None is measured; a value is set only after the single-node prototype of #2 §6 measures it.
- **The time a supervisor takes to end its namespace at the expiry**, under load and after a suspend; measured with the prototype numbers above. First observation, not a figure: with the test supervisor run as root in a container on an idle AMD Ryzen 5 9600X host (12 threads, Linux 6.12.115+deb13.1-amd64, the container sharing its kernel), a polling observer saw the process tree gone 3.5 to 7.0 ms after the expiry in 10 observations over two runs (expiry reached, mvmd's side gone, three renewals), the poll's own granularity (0.2 ms and a scan of `/proc`) included, and 4.7 and 5.7 ms after a SIGKILL of the supervisor. No load and no suspend; nothing here is a bound.
- **How many earlier releases' supervisors mvmd must speak to**, since a supervisor outlives an mvmd upgrade until its Machine is next started.
