# ADR 0030: The Guest Agent and converter initrds are pinned by digest, as the kernel is

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised while reviewing the Cloud Hypervisor Driver's restore (garamsh/mvmd#141) and filed as garamsh/mvmd#175.

[ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md) pins the kernel by digest and says the initrd is the Guest Agent's, or the converter's, without pinning it. Each Driver's configuration named the initrd by path and checked only that the file exists, and the boot description listed it with no digest, so the supervisor checked the kernel it presented to the VMM and not the initrd beside it.

The initrd is the guest's PID 1: the Guest Agent, or the converter. A different file at the configured path ran as the guest's init with no check, though the kernel next to it was checked. The initrd is reproducible: `make build` builds each twice and checks that both builds give one digest ([guest.md](../guest.md)), so a digest can be pinned.

[ADR 0027](./0027-a-restored-vmms-root-holds-the-boot-files-its-vmm-opens-on-restore.md) binds the initrd into a restored Cloud Hypervisor VMM's root without a digest because boot has none, and says in its Consequences that a pin would be a change to both.

Candidates weighed:

- **Pin each initrd by SHA-256 in the node's configuration, as the kernel is, and carry the digest in every description that lists the initrd** — chosen.
- **Leave the initrd unpinned.** The files the VMM opens would be checked for the kernel and not for the program that runs first.
- **Pin only at the Driver's start.** [ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md) puts the check of record on the file the supervisor presents to the VMM, since a path hashed by mvmd can be replaced before the supervisor opens it.

## Decision

The node's configuration names each initrd, the Guest Agent's and the converter's, with its SHA-256 in hex, as it names the kernel. A Driver refuses to start when the initrd's digest differs. Every description that lists an initrd states its digest in the file's `SHA256`, so the supervisor checks the file it mounts before every boot and every restore: the boot, the converter's boot, and the Cloud Hypervisor restore. The supervisor's check is the existing one on `machine.File.SHA256`; it changes nothing.

This extends the scope of ADR 0010's pin from the kernel to the kernel and the initrd. How the initrd is built does not change.

It changes ADR 0027's restore binding: the initrd the Cloud Hypervisor Driver binds at `/boot/initrd` on restore carries the digest the boot's carries.

## Consequences

- A node whose configuration names an initrd without its digest does not start. Existing configurations take the new field, `initrd_sha256`, in both the `firecracker` and `conversion` sections.
- A rebuild of the Guest Agent or the converter changes the initrd's digest, so the node's configuration changes with it, as a new kernel's does. `make test-firecracker`, `make test-cloud-hypervisor` and `make test-e2e` take the digest of the build they have just made.
- A restore that finds the initrd at the configured path with another digest is refused by the supervisor before the VMM starts, though the restored guest never runs the file ([ADR 0027](./0027-a-restored-vmms-root-holds-the-boot-files-its-vmm-opens-on-restore.md)). A snapshot still restores on a node whose initrd is the pinned one, whichever initrd the Machine booted from: the snapshot does not record the initrd, and the compatibility check does not change.
- A Driver added later pins its initrd as these two do.

## Signs the decision was wrong

- A node's initrd is rebuilt more often than its kernel is changed and the configuration cannot keep up, so nodes pin a stale digest or tooling writes the digest from whatever file is there, which makes the pin check nothing.
- A VMM opens the initrd by a route the supervisor's presented file does not cover, so the digest is checked on a file the VMM does not read.
