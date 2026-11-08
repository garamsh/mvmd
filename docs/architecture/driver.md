# VMM Driver

The contract between the mvmd inside and a virtual machine monitor, and what implements it first.

Checked against garamsh/discussions#2 as read on 2026-11-08, §5 (discussioncomment-19288321) and the decision-change reply (discussioncomment-19466961). Claims about what a VMM supports (the table below) are #2's, which marks the Firecracker entry as needing a version check.

## Current decisions

- **The VMM is swappable behind a Driver port.** Firecracker and Cloud Hypervisor are the first two Drivers; a VMM written later in-house must be admissible through the same contract (decision-change reply). The contract is stated in VMM-neutral terms and no VMM's vocabulary appears in it ([structure.md](./structure.md) §Boundaries and dependency direction).
- **Operations.** The contract covers create (configure without booting), start, stop, destroy, pause, resume, snapshot, restore, disk attach and detach, memory target, inspect, list, and an event stream (#2 §5, Driver interface draft). The draft's signatures are not transcribed here; the code is the source of truth once it exists.
- **Capability negotiation (#2 §5 design principle 1).** A VMM reports which optional operations it supports, and the client matches a Machine's required capabilities to a node. The contract is not cut to what every VMM supports.
- **Snapshot compatibility (principle 2).** A snapshot records the Driver name and version, the CPU model and the guest kernel. A snapshot does not move to another VMM, and the contract says so.
- **Idempotency and post-failure state (principle 3).** Create-type calls carry a request ID, and each operation defines the state a VM is left in when it fails.
- **Restart (principle 4).** The VMM runs as its own process; mvmd reattaches to running VMs after its own restart through `List`.
- **The Driver does not start the VMM process.** For start and restore it describes the process to run, and mvmd starts it under the Machine's supervisor, which enforces the fencing expiry ([runtime.md](./runtime.md) §Fencing). Every process a Machine needs runs under that supervisor, and none runs under a real-time scheduling policy. The Driver never sees the expiry, so a new VMM is fenced without fencing code of its own ([ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)).
- **In-process first (principle 5).** Drivers are Go implementations inside the daemon, with messages shaped so that they can move to gRPC. An out-of-process Driver protocol, in the role containerd's shim API plays, is introduced when the first out-of-process VMM, such as an in-house one, arrives (decision-change reply). It keeps the VMM process started by the supervisor.
- **Other driven ports.** Disk, Network, Snapshot Store and Image Store are interfaces with one implementation each at first (#2 §5, first implementation scope). They are ports in [structure.md](./structure.md) §Ports.

## VMM support for resizing

Per #2 §5, by VMM:

| VMM | vCPU hotplug | Memory |
|---|---|---|
| Cloud Hypervisor | supported | hotplug and virtio-mem supported |
| Firecracker | believed unsupported by default; version to be checked | balloon-centred |

Resize operations are optional capabilities because of this difference.

## Rationale

- Why Drivers are the one thing kept swappable, and why there is no container runtime option: #2 §5, isolation unit and runtime choice; [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md).
- Why two implementations: an abstraction with one implementation cannot be verified (#2 §5, first implementation scope).
- Why the Driver describes the VMM process instead of starting it: [ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md).

## Open questions

- Whether Firecracker supports vCPU hotplug in the version mvmd pins; #2 states it as unchecked.
- The Driver contract's wire shape for the out-of-process case; introduced when the first out-of-process VMM arrives.
- Which boot formats each Driver accepts for MachineImage; #2 §5 says this differs by VMM and is checked per Driver.
