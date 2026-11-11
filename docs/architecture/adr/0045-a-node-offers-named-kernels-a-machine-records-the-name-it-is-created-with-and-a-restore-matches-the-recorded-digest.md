# ADR 0045: A node offers named kernels, a Machine records the name it is created with, and a restore matches the recorded digest

> Status: accepted
> Date: 2026-11-12

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#364. [ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md) pins one kernel per node, in the Driver's section of the node configuration, and [ADR 0030](./0030-the-guest-agent-and-converter-initrds-are-pinned-by-digest-as-the-kernel-is.md) extends the pin to the initrd. A node that must move from one guest kernel to another has no way to run both: replacing the file changes what every Machine boots at its next start, and a snapshot restores only against the one kernel digest the node holds. The node OS project (naru) builds kernels and needs to roll one out beside the one in service.

Candidates weighed:

- **Keep one kernel and let a new kernel replace it.** Every Machine moves at its next start, with no way to keep one back and no record of which kernel a Machine was created for.
- **A kernel chosen per Start.** A Machine's kernel would then be a property of a call that can be repeated or lost, and a snapshot taken after would not say which kernel it holds.
- **A kernel named at Create, recorded with the Machine** — chosen. The name is a property of the Machine, like its resource ceiling, and a restore has a name and a digest to compare.

## Decision

1. **Node configuration.** A real Driver's section replaces `kernel` and `kernel_sha256` with `kernels`, a map from a name to `{path, sha256}`, and `default_kernel`, one of the names. A name matches `[a-z0-9-]{1,63}`. The Driver checks every listed kernel's digest when mvmd starts, and the supervisor checks the mounted one before every boot (ADR 0030's rule, unchanged). mvmd refuses to start on the old fields, or on a `default_kernel` that is not in the map, and the error names the field. The converter keeps its own single `conversion` kernel, which no Machine names.
2. **Runtime API `v1alpha2`, additive.** `CreateMachineRequest` takes `string kernel`; empty gets the node's default. The name is resolved and recorded at Create, and a name the node does not offer is refused as invalid with no Machine made. `Machine.kernel` reports the recorded name and `kernel_digest` stays as it was. Runtime info lists the node's kernels, each with its name, digest and whether it is the default. `Snapshot.kernel` reports the name a snapshot recorded.
3. **Start** boots the file the recorded name maps to at that moment. A name the node no longer offers is a failed precondition naming the kernel, and the Machine's record is kept. An empty recorded name, which only a record written before this decision has, means the default.
4. **Snapshot** records the kernel's name and its digest. **Restore** requires an offered kernel with the recorded digest: if none matches, it is a failed precondition and nothing is written. The restored Machine records the name of the kernel that matched; where several names share the digest, the recorded name is taken if offered, then the default, then the first by name. The Driver binds no kernel on a restore ([ADR 0027](./0027-a-restored-vmms-root-holds-the-boot-files-its-vmm-opens-on-restore.md)), and a restore is judged by the digest, not the path.
5. **The Driver is given the kernel in the boot description.** `BootSpec.Kernel` carries the path and digest, and a Driver's `Describe` states that file. A Driver no longer holds a single kernel for the lifecycle to ask about: its snapshot reports only its files.

## Consequences

- A node runs two kernels side by side, and a Machine stays on the one it was created with until it is deleted or the node stops offering the name.
- Removing a name from the configuration strands the Machines that recorded it: they fail to start, with the name in the error, until the name is offered again. Their records and disks are kept.
- A snapshot restores on any node that offers a kernel with its digest, under whatever name that node gives it.
- A node's configuration written for ADR 0010 no longer starts. naru replaces `kernel` and `kernel_sha256` with `kernels: {name: {path, sha256}}` and `default_kernel`.
- Machine and snapshot records written before this decision read back with an empty kernel name.

## Signs the decision was wrong

- A kernel's name is wanted to be changed on a living Machine, so a name fixed at Create was too early to bind.
- Restores are refused for want of a digest on nodes that hold an equivalent kernel built differently, so the digest was too strict a match.
- Two names for the same digest are used to mean different things, so a restore's choice between them matters.
