# VMM Driver

The contract between the mvmd inside and a virtual machine monitor, and what implements it first.

Checked against garamsh/discussions#2 as read on 2026-11-08, §5 (discussioncomment-19288321) and the decision-change reply (discussioncomment-19466961). Claims about what a VMM supports in the resizing table are #2's, which marks the Firecracker entry as needing a version check; claims about boot formats and disks are ADR 0009's and ADR 0010's, at Firecracker v1.17.0 and Cloud Hypervisor v54.0.

## Current decisions

- **The VMM is swappable behind a Driver port.** Firecracker and Cloud Hypervisor are the first two Drivers; a VMM written later in-house must be admissible through the same contract (decision-change reply). The contract is stated in VMM-neutral terms and no VMM's vocabulary appears in it ([structure.md](./structure.md) §Boundaries and dependency direction).
- **Operations.** The contract covers create (configure without booting), start, stop, destroy, pause, resume, snapshot, restore, disk attach and detach, memory target, inspect, list, and an event stream (#2 §5, Driver interface draft). The draft's signatures are not transcribed here; the code is the source of truth once it exists.
- **Capability negotiation (#2 §5 design principle 1).** A VMM reports which optional operations it supports, and the client matches a Machine's required capabilities to a node. The contract is not cut to what every VMM supports.
- **Snapshot compatibility (principle 2).** A snapshot records the Driver name and version, the CPU model and the guest kernel. A snapshot does not move to another VMM, and the contract says so.
- **Idempotency and post-failure state (principle 3).** Create-type calls carry a request ID, and each operation defines the state a VM is left in when it fails.
- **Restart (principle 4).** The VMM runs as its own process; mvmd reattaches to running VMs after its own restart through `List`.
- **The Driver does not start the VMM process.** For start and restore it describes the process to run, and mvmd starts it under the Machine's supervisor, which enforces the fencing expiry ([runtime.md](./runtime.md) §Fencing). Every process a Machine needs runs under that supervisor, and none runs under a real-time scheduling policy. The Driver never sees the expiry, so a new VMM is fenced without fencing code of its own ([ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)).
- **In-process first (principle 5).** Drivers are Go implementations inside the daemon, with messages shaped so that they can move to gRPC. An out-of-process Driver protocol, in the role containerd's shim API plays, is introduced when the first out-of-process VMM, such as an in-house one, arrives (decision-change reply). It keeps the VMM process started by the supervisor.
- **Boot artefacts ([ADR 0010](./adr/0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)).** A Driver boots a VM from: a kernel file (on x86_64 an uncompressed ELF kernel with a PVH entry, on aarch64 a PE `Image`, both taken by Firecracker and Cloud Hypervisor), an initrd file, a kernel command line, an ordered list of block devices each a file path and a read-only flag, one vsock device, and the vCPU count and memory size. A Machine's devices are its image's EROFS read-only, its root disk read-write, then its attached disks; a converter's are its output disk alone. The Driver learns nothing of what each device holds.
- **Disks are raw.** Every disk is a raw file, and a Driver whose VMM takes a disk format names raw, so nothing a guest writes selects a format ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)).
- **The command line** carries mvmd's guest parameters; a Driver may add what its VMM's devices need and removes none.
- **The converter is booted through the Driver** the node's configuration names for conversion, with no optional capability ([ADR 0005](./adr/0005-untrusted-images-are-converted-in-a-converter-microvm.md)).
- **Other driven ports.** Disk, Network, Snapshot Store and Image Store are interfaces with one implementation each at first (#2 §5, first implementation scope); Registry and Converter channel join them for image pull ([ADR 0005](./adr/0005-untrusted-images-are-converted-in-a-converter-microvm.md)). They are ports in [structure.md](./structure.md) §Ports.

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
- The boot formats of a VMM admitted later; each new Driver is checked against the artefacts above.
