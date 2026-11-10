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
- **A node-wide ceiling.** Rejected: it sizes every VMM's reserve for the largest Machine the node allows.
- **No ceiling, the VMM started with the largest figure the node allows.** Rejected: the same reservation for every Machine, and the cgroup would bound nothing.

## Decision

- **A Machine has a resource ceiling, the most a live resize of it reaches.** `Spec` carries `Ceiling`, a vCPU count and a memory size in bytes. The runtime API carries it as `resource_ceiling` on create, set resources, and the Machine, and the response of set resources.
- **A figure of the ceiling left unset at create is the resources' figure,** so a Machine created with none has a ceiling equal to its resources. Get returns the effective value, never an unset one.
- **Resources above the ceiling are `invalid`,** at create and at set resources, in either figure.
- **At set resources a ceiling figure left unset leaves the Machine's figure as it is.** Update and create differ on purpose: at create there is nothing to leave.
- **A ceiling change on a Running or Paused Machine is `precondition failed`.** A VMM's limits are fixed when it starts, so the ceiling changes only while the Machine is Created or Stopped, and applies at its next start.
- **A ceiling above the resources on a Driver without the resize capability is `unsupported`,** at create and at set resources.
- **A hot target below the memory the VMM booted with is `precondition failed`, and the reason says stopping the Machine applies it.** The Machine holds `BootMemoryBytes`, set when it becomes Running; a snapshot records it and a restore sets it from the snapshot. A record written before it existed reads back with the memory of its resources.
- **The guest figures in the process description are the ceiling.** `BootSpec` carries `Ceiling`; `ProcessDescription.For` takes the guest from it, falling back to the resources when it is unset. The supervisor's cgroup is therefore sized for the most the guest can have; the `supervisor.proto` comment on `guest_memory_bytes` and `guest_vcpus` says so, and the message is otherwise unchanged.
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
