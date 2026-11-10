# ADR 0029: A Machine has a resource ceiling chosen at create, which bounds a live resize and the VMM's limits

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#176 and implemented by garamsh/mvmd#187. The research it rests on is in garamsh/mvmd#176 (comment 6755614122).

- A live resize of a Running or Paused Machine is bounded by what the VMM was started with. Measured on Cloud Hypervisor v54.0 (#176, sections 1 and 2): with virtio-mem, a resize is refused below the boot size, above the region fixed at start (`hotplug_size`), and off a 2 MiB step; a vCPU count above `max` is refused. Both limits are fixed when the VMM starts.
- The supervisor's cgroup takes `memory.max` and `cpu.max` from `ProcessDescription.Guest` (runtime.md §The VMM's process). A resize above those figures would be killed or throttled by the cgroup rather than refused.
- Until now a Machine's resources were the one figure, so no live resize could exceed what the Machine was created with, and the Driver's `Resize` had no range to name.
- A Driver without the resize capability cannot use a ceiling above the resources, so accepting one would promise a range that can never be reached.

Candidates weighed:

- **A ceiling chosen at create, held on the Machine** — chosen.
- **A node-wide ceiling (option A).** Rejected: every Machine pays for it, about 10 MB of VMM RSS and 6.8 MB of guest memory at 32 vCPUs (#176, section 4). It is also not part of the Machine's record, so a change to the node leaves running and snapshotted Machines inconsistent with the configuration. A node-wide default can be added on top of a per-Machine ceiling later; the reverse cannot be recovered.
- **`max` equal to `boot`, no ceiling (option C).** Rejected: Cloud Hypervisor could then not grow a Machine at all, and the goal does not allow skipping resize on it. The cold path stays as it is: a target outside what a running VMM can reach applies at the next start.
- **ACPI memory hotplug instead of virtio-mem.** Rejected: it cannot shrink. A shrink answers 204, leaves the guest unchanged, and leaves the VMM's `config.memory.size` disagreeing with the guest (#176, section 2). virtio-mem shrinks, refuses a target below the boot memory, and steps in 2 MiB.

## Decision

- **A Machine has a resource ceiling, the most a live resize of it reaches.** `Spec` carries `Ceiling`, a vCPU count and a memory size in bytes. The runtime API carries it as `resource_ceiling` on create, set resources, and the Machine, and the response of set resources.
- **A figure of the ceiling left unset at create is the resources' figure,** so a Machine created with none has a ceiling equal to its resources. Get returns the effective value, never an unset one.
- **Resources above the ceiling are `invalid`,** at create and at set resources, in either figure.
- **At set resources a ceiling figure left unset leaves the Machine's figure as it is.** Update and create differ on purpose: at create there is nothing to leave.
- **A ceiling change on a Running or Paused Machine is `precondition failed`.** A VMM's limits are fixed when it starts, so the ceiling changes only while the Machine is Created or Stopped, and applies at its next start.
- **A ceiling above the resources on a Driver without the resize capability is `unsupported`,** at create and at set resources. On such a Driver the held ceiling always equals the resources: a set resources with no ceiling moves the ceiling with the target, in either direction, so the VMM's cgroup is never sized for a guest that can never reach it. A Driver that can resize keeps the ceiling it holds.
- **A hot target below the memory the VMM booted with is `precondition failed`, and the reason says stopping the Machine applies it.** The Machine holds `BootMemoryBytes`, set when it becomes Running; a snapshot records it and a restore sets it from the snapshot. A record written before it existed reads back with the memory of its resources.
- **The guest figures in the process description are the ceiling.** `BootSpec` carries `Ceiling`; `ProcessDescription.For` takes the guest from it, falling back to the resources when it is unset. The supervisor's cgroup is therefore sized for the most the guest can have; the `supervisor.proto` comment on `guest_memory_bytes` and `guest_vcpus` says so, and the message is otherwise unchanged.
- **Cloud Hypervisor boots with the ceiling, and a resize reaches it** (item 6 of the decision on #176; implemented by #176, not here).
  - Boot: `--cpus boot=N,max=<ceiling>` and `--memory size=<resources>,hotplug_method=virtio-mem,hotplug_size=<ceiling minus resources>`. The hotplug part is omitted when the ceiling equals the resources.
  - `Resize` sends `PUT /vm.resize` and waits until the VMM reports the target: `memory_actual_size` for memory, and the vCPU count. A timeout is `failed`, and a retried resize converges, as detach does after #177.
  - `Info` reports `resize` only on x86_64: Cloud Hypervisor documents CPU hotplug as x86 only (`docs/hotplug.md`), and `resize` holds for both resources or is not reported.
- **The guest brings new vCPUs online** (item 7 of the decision on #176, with its amendment; the operation is merged in #189, the call is #176's).
  - The Guest Agent's `OnlineVcpus(target)` brings present, offline vCPUs online only up to the target, writes nothing at or above it, and refuses a target of 0. It answers with the online vCPU count and the guest's memory.
  - After `Driver.Resize` the lifecycle calls it with the target, as it calls `FindDisk` after `AttachDisk` (ADR 0022), and waits until the reported count equals the target. A guest that fails the call fails the resize. Removing vCPUs needs no guest action, and the report still confirms the new count. The kernel auto-onlines new memory, so memory needs no call.
- **The memory alignment is enforced in the lifecycle, not in a Driver.** A memory figure of the resources or the ceiling that is not a multiple of 2 MiB (`MemoryAlignment`) is `invalid`, so every Driver sees only aligned figures.
- **A snapshot carries the ceiling, and a restore uses the snapshot's.** A record in either store written before the ceiling existed reads back with the ceiling equal to its resources.

## Consequences

- A live resize has a stated range, so the Driver's `Resize` and the cgroup agree on it.
- A VMM reserves for its ceiling, not its resources: a Machine created with a high ceiling costs the node that reserve while it runs.
- A shrink below the boot memory is possible only by stopping the Machine, so an operator who wants a smaller Machine waits for a stop.
- The 2 MiB rule is stricter than Firecracker needs, and applies to it too, so one rule holds for every Driver.
- Both stores and the API gain fields; the stored forms are read back with the defaults above, so no migration is run.

## Signs the decision was wrong

- Operators routinely create Machines with a ceiling far above what they use, and the node's memory is held by reserves that are never reached.
- A VMM is found that resizes past its start-time limits, so the ceiling bounds nothing for it.
- A Driver is found that needs a different alignment than 2 MiB, so the lifecycle's rule has to learn a per-Driver figure.
- Changing the ceiling of a running Machine becomes a common request, and stopping the Machine for it is the complaint.
