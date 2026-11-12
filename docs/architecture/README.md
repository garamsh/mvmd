# Architecture

Current architecture of the system and the decisions behind it. This file is the index; workers read it before touching code, the PM keeps it honest.

## Structure

- **Responsibility documents** (one `.md` per domain or concern, e.g. `memory.md`, `gateway.md`) — the single source of truth for how the system is shaped *now*. To know the current state, read only these.
- **`adr/`** — the record of individual decisions: direction taken, context, rejected alternatives.

## Rules

1. **Responsibility document skeleton**: current decisions (consolidated) / rationale summary with links to the relevant ADRs / open questions. Do not copy ADR content — synthesize the present state.
2. **ADRs are append-only.** Once merged, the body is frozen. Exceptions: updating the status field (`accepted` → `superseded by ADR-XXXX`), fixing typos or broken links, and appending a dated entry under `## Errata` when the decision stands but a fact supporting it was wrong — the erratum names what falsified it, and the original text stays intact. A changed decision means a new ADR that supersedes the old one — never an edit.
3. **Decisions land in pairs.** A PR that adds or supersedes an ADR must update the affected responsibility documents in the same PR. A PR with only one of the two is rejected — the final state must always live in the responsibility documents.
4. Keep this index current: every responsibility document and every ADR is listed here. An ADR covers a decision that changes a project rule — a settled choice that does not affect the rules does not earn one.
5. **Every domain or concern has a responsibility document.** The PR that introduces one adds its document; the PR that removes one deletes it. A system with no code yet has no more than `structure.md` (rule 6), and that is the correct state.
6. **Structural rules live in `structure.md`.** The shape chosen at bootstrap imposes rules — what the unit is, what may reference what, when a new one is earned — and they belong in the responsibility document for that concern, where a reviewer cites one by line and a correction is an ordinary edit. Not the ADR: rule 2 freezes that body, so superseding the choice of shape would be the price of fixing a rule's wording.
7. **A point the structural rules leave to the project is settled in an ADR** — the one recording the shape where the question is met at bootstrap, a later one where it is met later. Which side is taken decides what counts as a defect, so it is a decision under rule 4, and the ADR still holds the record after bootstrap. Nothing obliges a project to settle a question it has not met: until then the point waits under `structure.md`'s open questions, where it binds nothing, and moves into that document's current decisions when the project takes a side.
8. **Structural rules yield only to what a framework imposes.** Where `structure.md` and a convention file decide the same case differently, `structure.md` governs, unless the stack-specific file that applies decides it as its framework imposes it — the alternative fails to build or run, so the project cannot restate it. A stack file's spelling of a structural rule is not that: the rule it spells is `structure.md`'s. The shape chosen at bootstrap is this project's own refinement of what a stack-neutral file states for every project. A project with no `structure.md` meets no such conflict, and this rule asks nothing of it.

## Index

Responsibility documents:

- [structure.md](./structure.md) — the hexagonal shape, repository layout, boundaries and dependency direction, ports
- [runtime.md](./runtime.md) — what mvmd owns on a node, its boundary with gaon, fencing, the node network, VMM confinement, registry access, invariants
- [runtime-api.md](./runtime-api.md) — the runtime API: operations, calling rules, version v1alpha2, fencing deadline
- [driver.md](./driver.md) — the VMM Driver contract
- [image.md](./image.md) — image pull and conversion, MachineImage, node image cache, integrity
- [guest.md](./guest.md) — the Guest Agent, guest boot order, guest kernel scope

Decisions:

- [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md) — mvmd is a containerd-like microVM runtime that does not know gaon
- [ADR 0002](./adr/0002-mvmd-is-structured-as-a-hexagonal-application.md) — mvmd is structured as a hexagonal application
- [ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md) — a per-Machine supervisor enforces the fencing expiry
- [ADR 0004](./adr/0004-the-runtime-api-is-grpc-on-a-unix-socket-with-committed-generated-code.md) — the runtime API is gRPC on a Unix socket, with committed generated code
- [ADR 0005](./adr/0005-untrusted-images-are-converted-in-a-converter-microvm.md) — untrusted images are converted in a converter microVM
- [ADR 0006](./adr/0006-mkfs-erofs-converts-an-image-one-layer-at-a-time.md) — mkfs.erofs converts an image one layer at a time
- [ADR 0007](./adr/0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md) — the node image cache is an OCI image layout, published by one index swap
- [ADR 0008](./adr/0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md) — fs-verity on the host guards every read of a MachineImage
- [ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md) — a Machine's root disk is a sparse raw file with one holder (the location of the root disk is amended by ADR 0021)
- [ADR 0010](./adr/0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md) — a Machine boots a pinned kernel and the Guest Agent initrd from raw block devices (the pin is extended to the initrd by ADR 0030)
- [ADR 0011](./adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md) — each Machine gets a routed tap and addresses mvmd assigns
- [ADR 0012](./adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md) — nftables on the host enforces each Machine's policy at its tap
- [ADR 0013](./adr/0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md) — a network policy is an allow-list of address, protocol and port rules
- [ADR 0014](./adr/0014-the-guest-agent-serves-a-grpc-contract-it-owns-over-vsock.md) — the Guest Agent serves a gRPC contract it owns over vsock
- [ADR 0015](./adr/0015-the-supervisor-confines-every-vmm-process-the-same-way.md) — the supervisor confines every VMM process the same way (the names of the files in the VMM's mount namespace are amended by ADR 0019)
- [ADR 0016](./adr/0016-a-restored-guest-reseeds-from-host-entropy-and-takes-its-time-from-the-host.md) — a restored guest reseeds from host entropy and takes its time from the host (the clock is also set at a boot, by ADR 0044)
- [ADR 0017](./adr/0017-the-converter-serves-a-grpc-contract-it-owns-and-carries-mkfs-erofs-in-its-initrd.md) — the converter serves a gRPC contract it owns and carries mkfs.erofs in its initrd
- [ADR 0018](./adr/0018-a-graceful-stop-goes-through-the-guest-agent-and-the-vmm-request-is-the-fallback.md) — a graceful stop goes through the Guest Agent, and the VMM request is the fallback
- [ADR 0019](./adr/0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md) — a VMM sees its Machine's files at fixed paths in its root, and the supervisor presents the file it checked
- [ADR 0020](./adr/0020-the-supervisor-serves-the-vmms-serial-console-on-a-socket-outside-its-root.md) — the supervisor serves the VMM's serial console on a socket outside its root (the choice of pipes for the VMM's standard input and output is superseded by ADR 0025)
- [ADR 0021](./adr/0021-a-root-disk-is-a-file-in-the-disks-directory-of-mvmds-state-directory.md) — a root disk is a file in the disks directory of mvmd's state directory
- [ADR 0022](./adr/0022-the-guest-finds-a-hot-plugged-disk-by-rescan-and-releases-it-before-the-unplug.md) — the guest finds a hot-plugged disk by rescan and releases it before the unplug (the rule for which device `FindDisk` takes is superseded by ADR 0028)
- [ADR 0023](./adr/0023-a-restart-settles-a-running-machines-disks-against-the-vmms-drives.md) — a restart settles a Running Machine's disks against the VMM's drives
- [ADR 0024](./adr/0024-the-converters-user-holds-the-output-file-only-while-its-vmm-runs.md) — the converter's user holds the output file only while its VMM runs (adds to the steps 6 and 7 of ADR 0007's Pull image)
- [ADR 0025](./adr/0025-the-supervisor-gives-the-vmm-a-raw-mode-pseudo-terminal-as-its-console.md) — the supervisor gives the VMM a raw-mode pseudo-terminal as its console (supersedes ADR 0020's pipes)
- [ADR 0026](./adr/0026-the-supervisor-ends-a-vmm-whose-event-pipe-reports-a-guest-reset.md) — the supervisor ends a VMM whose event pipe reports a guest reset (extends what ADR 0015's description carries)
- [ADR 0027](./adr/0027-a-restored-vmms-root-holds-the-boot-files-its-vmm-opens-on-restore.md) — a restored VMM's root holds the boot files its VMM opens on restore (replaces driver.md's rule that a restored root holds no kernel and no initrd; the initrd it binds carries its digest since ADR 0030)
- [ADR 0028](./adr/0028-find-disk-takes-the-one-virtio-block-device-the-agent-has-not-accounted-for.md) — `FindDisk` takes the one virtio block device the agent has not accounted for (supersedes ADR 0022's rule for which device `FindDisk` takes)
- [ADR 0029](./adr/0029-a-machine-has-a-resource-ceiling-chosen-at-create-which-bounds-a-live-resize-and-the-vmms-limits.md) — a Machine has a resource ceiling chosen at create, which bounds a live resize and the VMM's limits
- [ADR 0030](./adr/0030-the-guest-agent-and-converter-initrds-are-pinned-by-digest-as-the-kernel-is.md) — the Guest Agent and converter initrds are pinned by digest, as the kernel is (extends ADR 0010's pin from the kernel to the initrd, and ADR 0027's restore binding)
- [ADR 0031](./adr/0031-the-snapshot-store-publishes-a-snapshots-state-files-owned-by-mvmd-and-read-only.md) — the Snapshot Store publishes a snapshot's state files owned by mvmd and read-only
- [ADR 0032](./adr/0032-the-guest-agent-ends-the-machine-by-the-method-the-kernel-command-line-names.md) — the Guest Agent ends the Machine by the method the kernel command line names
- [ADR 0033](./adr/0033-a-vmm-that-ends-on-its-own-moves-its-machine-to-stopped-with-the-reason.md) — a VMM that ends on its own moves its Machine to Stopped with the reason (the daemon finds it through the supervisors it lists)
- [ADR 0034](./adr/0034-the-node-reclaims-nothing-by-its-own-policy-and-the-runtime-api-lists-and-removes-what-it-holds.md) — the node reclaims nothing by its own policy, and the runtime API lists and removes the snapshots and images it holds
- [ADR 0035](./adr/0035-the-runtime-api-streams-listings-and-searches-and-serves-one-alpha-version-at-a-time.md) — the runtime API streams listings and searches and serves one alpha version at a time (`v1alpha2`)
- [ADR 0036](./adr/0036-the-runtime-api-creates-lists-and-deletes-a-node-local-disk-through-the-disk-adapter.md) — the runtime API creates, lists and deletes a node-local disk through the Disk adapter
- [ADR 0037](./adr/0037-a-restores-guest-catch-up-is-bounded-by-the-boot-timeout-as-a-starts-boot-is.md) — a restore's guest catch-up is bounded by the boot timeout, as a start's guest boot is
- [ADR 0038](./adr/0038-a-disk-file-belongs-to-the-machine-that-holds-it.md) — a disk file belongs to the Machine that holds it (completes what ADR 0036 left open about a VMM opening a Create disk disk)
- [ADR 0039](./adr/0039-runtime-info-reports-the-nodes-capacity-and-mvmd-admits-no-call-by-it.md) — runtime info reports the node's capacity, and mvmd admits no call by it
- [ADR 0040](./adr/0040-a-driver-states-the-most-of-a-machine-it-accepts-and-the-inside-refuses-a-request-above-it.md) — a Driver states the most of a Machine it accepts, and the inside refuses a request above it
- [ADR 0041](./adr/0041-a-snapshot-records-its-disks-a-restore-takes-exactly-those-and-a-non-root-disk-reaches-the-guest-by-hot-plug.md) — a snapshot records its disks, a restore takes exactly those, and a non-root disk reaches the guest by hot-plug (decision 3 superseded by ADR 0042)
- [ADR 0042](./adr/0042-the-supervisor-links-the-files-a-process-description-lists-into-the-run-directory-before-the-vmm-starts.md) — the supervisor links the files a process description lists into the run directory before the VMM starts
- [ADR 0043](./adr/0043-an-exited-command-carries-the-time-the-guest-agent-reaped-it-by-the-guests-clock.md) — an exited command carries the time the Guest Agent reaped it, by the guest's clock
- [ADR 0044](./adr/0044-boot-sets-the-guests-wall-clock-to-the-hosts-as-catchup-does.md) — Boot sets the guest's wall clock to the host's, as CatchUp does (it amends the scope of ADR 0016's clock from a restore to a boot and a restore)
- [ADR 0045](./adr/0045-a-node-offers-named-kernels-a-machine-records-the-name-it-is-created-with-and-a-restore-matches-the-recorded-digest.md) — a node offers named kernels, a Machine records the name it is created with, and a restore matches the recorded digest (it replaces ADR 0010's one kernel per node with a named set)
- [ADR 0046](./adr/0046-a-bound-the-node-is-configured-with-is-reported-and-refused-as-precondition-failed-while-measured-capacity-stays-the-clients.md) — a bound the node is configured with is reported and refused as precondition failed, while measured capacity stays the client's (it extends ADR 0039)
- [ADR 0047](./adr/0047-a-pull-reads-the-registry-credentials-and-ca-at-each-lookup-and-takes-a-credential-from-a-helper.md) — a pull reads the registry credentials and CA at each lookup, and takes a credential from a helper
- [ADR 0048](./adr/0048-the-guest-agent-runs-the-entrypoint-as-a-named-user-and-places-files-from-the-boot-call-in-a-tmpfs-it-alone-mounts.md) — the guest agent runs the entrypoint as a named user and places files from the Boot call in a tmpfs it alone mounts

The template for a new ADR is [adr/0000-template.md](./adr/0000-template.md).
