# ADR 0019: A VMM sees its Machine's files at fixed paths in its root

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#85, from the review of the pull request that adds the Firecracker Driver (garamsh/mvmd#84). This ADR amends two places of [ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md) and leaves the rest of it as it is: the "Its own mount namespace" step, which bind-mounted each file of the boot description at its own host path, and the "Kernel, initrd, disks" row of its table, which named those paths. ADR 0015's body stays frozen.

What went wrong with host paths, as garamsh/mvmd#85 records it from garamsh/mvmd#84, at Firecracker v1.17.0:

- A Firecracker snapshot records each drive's `path_on_host`, and `PUT /snapshot/load` fails when a recorded path does not exist in the new VMM's root (`Error manipulating the backing file: No such file or directory`).
- A restored Machine's root disk is a new file at a new host path, and its run directory is a new directory ([ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)), so a restore cannot work while the VMM sees host paths. The vsock device's `uds_path` and any hot-plugged disk's link are in the run directory too.
- Firecracker's snapshot load takes an override for the network device (`network_overrides`) and, as that issue records, none for drives.

A second defect, raised on garamsh/mvmd#85 from the same review: the Firecracker Driver hashed the pinned kernel and binary by path in its description and the supervisor opened the same paths later, so a file replaced in between passed the check [ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md) asks for ("checks the digest before every boot"). The supervisor is where each file is presented to the VMM, so this ADR changes it in the place where the presentation changes.

What the check was made on, not read from documentation: on Linux 6.12.115, the kernel of the host the change was written on, `open_tree` with `OPEN_TREE_CLONE` and `AT_EMPTY_PATH` on a descriptor of a file or a directory yields a detached bind mount of it, and `move_mount` with `MOVE_MOUNT_F_EMPTY_PATH` attaches it. Run in a new user and mount namespace as an ordinary user, the sequence put a file at its path in a new root and not at its host path, and refused a file whose digest differed. Both calls need `CAP_SYS_ADMIN`, which the supervisor's child holds until it execs the VMM (ADR 0015, Consequences); the real root's behaviour is checked by the supervision tests, which run in the pipeline.

Candidates weighed, for how a restored Machine's files reach the paths a snapshot recorded:

- **Fixed names in the VMM's root, the same for every Machine** — chosen. It is the jailer's own model, a root with fixed relative names, and it needs nothing of the VMM beyond naming its devices by those paths.
- **Rewriting the paths in the snapshot's state at restore.** The state is the VMM's own format, written by one release and read by the same; rewriting it makes mvmd parse every Driver's state format and ties restore to each release's layout, where [driver.md](../driver.md) keeps the contract in VMM-neutral terms.
- **A VMM's own override at restore** (Firecracker's `network_overrides`). It exists for the network device only, so each Driver would carry a special case for drives and sockets, and a VMM admitted later may have none.
- **The restored disk at the path the snapshot recorded, on the host.** Two Machines restored from one snapshot, or a restore while the Machine it was taken from still exists, need the same host path, which ADR 0016's clones make ordinary.

For how each device is named, by what the name stays equal across the source Machine and the restored one:

- **The device's position in the boot description's devices** — chosen. A Machine's devices are its image, its root disk, then its attached disks in the order attached; a converter's is its output disk alone (ADR 0010). The Driver learns nothing of what a device holds, which ADR 0010 lists among the signs that decision was wrong.
- **A role name (`image`, `root`).** The Driver would have to know which device is which, and a converter has neither.
- **The disk's identifier.** It names a disk, and a restored Machine's root disk is a new one.

For how the check and the presentation become one:

- **The supervisor hashes the file it opened and mounts that same open file** — chosen.
- **The Driver hashes and the supervisor mounts the path.** The window this ADR exists to close.
- **The Driver passes a descriptor to the supervisor.** The two are processes joined by a gRPC conversation on a Unix socket ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)), which carries no descriptor.
- **A sealed in-memory copy of each pinned file.** It copies the kernel and the VMM at every start, and a VMM that is told a path needs a file system path to it in any case.
- **fs-verity on the pinned files**, as [ADR 0008](./0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md) does for images. It would close what the decision below leaves open, and needs a file system that has it under the node's kernel and binary; this ADR does not require one.

## Decision

Each file of a Machine's boot description is bind-mounted in the VMM's root at a path chosen by its role, and the Driver describes devices to its VMM by those paths only. The paths are defined once, in `daemon/internal/machine/`, next to `ProcessDescription`. The Driver takes them from there, and so does the Supervision adapter that sends them to the supervisor, so the two cannot disagree.

| File | Path in the VMM's root |
|---|---|
| The guest kernel | `/boot/kernel` |
| The initrd | `/boot/initrd` |
| The block device at position `n` of the boot description's devices | `/disk/n` |
| The Machine's run directory | `/run` |

- **What is not renamed.** The VMM executable stays at its host path, since nothing a VMM records names it, and so do the host files and device nodes the Driver names, whose paths the kernel and the VMM release decide and which are the same for every Machine already.
- **What the VMM is told.** Every path in its arguments and in its API requests is one of these, including the API socket, the log, the vsock socket and the devices. mvmd reaches the sockets by the run directory's host path. A snapshot then names no host path, and a restore binds the new Machine's own files to the same paths.
- **What the description carries.** For each boot file its host path, its path in the root, its mode and, for a pinned file, its SHA-256. A pinned executable carries its SHA-256 in the description as well. The supervisor's request carries the run directory's path in the root beside its host path. The conversation's fields are only added ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md), `supervisor.proto`); a supervisor of an earlier release ignores the new ones, mounts at host paths, and the VMM then does not find the files it was told, which fails the start and not the confinement. A supervisor refuses a request without them.
- **What the supervisor refuses.** A path in the root that is not absolute and clean, which is how none leaves the root; the root itself and anything under `/dev`, which the root's own `/dev` replaces; two host files at one path; one path in two modes; a path inside another. Landlock's rules are written for the paths in the root, as ADR 0015 says, and not for the host's.
- **The check and the presentation.** In the child that builds the root, the supervisor opens each host path once. For a pinned file it hashes the open file, and refuses the start, before the VMM runs, if the digest differs. It then clones the mount from that open descriptor (`open_tree`, `move_mount`) and never resolves the path again, so the file mounted is the file hashed and a file put at the path after the open is never the file the VMM is given. On Linux 6.12.115 the mount of a file that was replaced meanwhile fails, since the kernel refuses to mount an unlinked file, so the start fails and nothing is mounted. This replaces the Driver's hash in each Describe. The Driver still checks the pinned binary and kernel when mvmd starts, so a node fails at its start; the check of record is the supervisor's, at every start, the converter's included.
- **What stays open.** A write into the very file after the hash. ADR 0015 keeps the kernel and the VMM executable not writable by the VMM's user, so only a privileged host process can make one, and ADR 0015 confines a VMM against its guest, not against the host's own privileged processes. Closing it needs the file immutable (fs-verity, above), and no Machine's disk, which is written, can be pinned.

## Consequences

- A snapshot taken on one Machine restores into another, on this node or a later one, since nothing in it names a host path. The Firecracker Driver's snapshot and restore, which garamsh/mvmd#30 left for this, can be written.
- The supervisor and the Driver agree on names through `daemon/internal/machine/`; a new Driver names its VMM's devices by them and describes no host path to its VMM.
- A VMM that records the path of a file in its state sees the same path on every Machine; a file attached later than the boot description gets the position after the last, which a restore must bind in the same order.
- Every start reads the pinned kernel and binary once more, in the supervisor, where Describe read them before; the Driver's own hash on each Describe is gone.
- The unprivileged real-Firecracker tests run the VMM in a root of this shape (a mount namespace in a user namespace, or under root's where the host refuses that) in place of the host's own root, so the Driver is exercised against a root with no `/proc` and no `/sys`.
- ADR 0015's confinement steps, their order and its table's other rows stand. Its mount step now names the files by the table above.

## Signs the decision was wrong

- A VMM admitted later records, outside its devices and sockets, a path of a file mvmd places, which fixed names for these files do not cover.
- Position in the device list stops being a stable name, for instance when disks are attached and detached out of order across a snapshot and restore.
- Describe's digest check is needed before a supervisor exists, so that the check at the supervisor comes too late to refuse a Machine's creation cheaply.
- A file the pinned digests cover is replaced in place and a VMM runs it, so the check cannot rest on the file's owner alone.
