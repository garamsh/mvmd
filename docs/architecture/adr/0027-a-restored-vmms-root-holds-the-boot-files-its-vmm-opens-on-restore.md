# ADR 0027: A restored VMM's root holds the boot files its VMM opens on restore

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised while implementing the Cloud Hypervisor Driver's snapshot and restore (garamsh/mvmd#141). [driver.md](../driver.md) stated, for the Firecracker Driver, that the description of a restore lists no kernel and no initrd, since a restored guest has already loaded both. [ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md) puts the files a VMM opens at fixed paths in its root, and a restored VMM's root holds the snapshot's files and the Machine's own devices. The rule held for the one VMM that had a restore.

**What the evidence is.** The source of Cloud Hypervisor at the v54.0 tag:

- A restore rebuilds the VM from the snapshot's `config.json` (`vmm/src/lib.rs` lines 2669 to 2670). `RestoreConfig` has no field that overrides the payload, so the kernel and initramfs paths the snapshot recorded are the ones used (`vmm/src/config.rs` lines 2993 to 3010).
- `Vm::new` runs on the restore path as well, with `Some(&snapshot)` (`vmm/src/lib.rs` lines 2122 to 2139). It opens the recorded initramfs with `File::open`, and a file that is not there fails the restore with `Error::InitramfsFile` (`vmm/src/vm.rs` lines 694 to 702). The open is not conditioned on there being no snapshot. The initramfs is not loaded into guest memory on this path.
- The kernel is opened only when the build has the `tdx` feature (`vmm/src/vm.rs` lines 680 to 691); whether the pinned build has it was not checked, and the Driver binds no kernel, so a build with it would fail the restore and show in the real test.
- A failed restore ends the VMM: the error reaches `exit_evt`.

So a Cloud Hypervisor restore needs the initrd at the path the boot used, and needs no kernel. The initrd file is opened and never read into the guest.

Candidates weighed:

- **The description binds exactly the boot files the VMM opens on restore, each at its fixed path** — chosen. The contract states the rule and each Driver states which files it is.
- **Rewriting `config.json` before the restore.** It changes a file the Snapshot Store holds read-only, and ties mvmd to the VMM's internal format, which [ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md) rejected for the paths in the state.
- **An empty file at `/boot/initrd`.** It satisfies the open with a file that is not the initrd and hides why a file is there.

## Decision

`DescribeRestore` binds, read-only and at their fixed paths, exactly the boot files its VMM opens on restore, and no others. It names them as `Describe` does, so the supervisor mounts them the same way. A boot file that a Driver pins is pinned in the restore's description too, and the supervisor checks it on the file it mounts as it does at boot.

- The Firecracker Driver lists none: its load opens the snapshot's state and memory only, as before.
- The Cloud Hypervisor Driver lists the initrd at `/boot/initrd`, read-only, the file the node's configuration names, which is the file its boot description lists. The node pins no digest for the initrd, so the description carries none, as the boot's does not. The restored guest never runs the file.
- The snapshot still records no host path, and the compatibility check does not change: the initrd is neither recorded nor compared, because its contents never reach the restored guest.

For the Cloud Hypervisor Driver the restore is the process's own command line, `--restore source_url=file:///snapshot,resume=true`, with `net_fds=[net0@[3]]` when the Machine has a tap, since the HTTP restore cannot carry a descriptor. The boot's `--net` therefore gives the device an explicit `id=net0`, which `net_fds` matches by (`vmm/src/config.rs` lines 3116 to 3200). The VMM creates the VM as it starts, so `Restore` waits for the VM to be `Running`, as `Boot` does. The event monitor is set up before the restore branch (`cloud-hypervisor/src/main.rs` lines 722 to 784), so the description of a restore carries `--event-monitor fd=7` and the supervisor's end of a VMM on a guest reset ([ADR 0026](./0026-the-supervisor-ends-a-vmm-whose-event-pipe-reports-a-guest-reset.md)) applies to a restored VMM too.

## Consequences

- A restored Cloud Hypervisor VMM's root holds an initrd the guest never runs. The Machine's restore depends on the node's initrd file being there, as its boot does, and on nothing about its contents.
- A snapshot taken on a node restores on a node whose initrd file is any file at the configured path. A node whose configuration names no initrd cannot start the Driver, so no restore meets that case.
- The shared scenario in `daemon/internal/drivertest/` takes the number of files in a snapshot and the boot files of a restored root from the Subject, and holds each Driver to its own exactly, so a Driver that binds a file it does not need or omits one it does is found.
- A VMM added later states its own boot files on restore, and a VMM that opens none has the Firecracker answer.
- The initrd is bound without a digest, where the PM's decision on garamsh/mvmd#141 says each file is "pinned and checked as at boot". Boot pins none for the initrd either, so the restore matches the boot; a pin for the initrd would be a change to both.

## Signs the decision was wrong

- A later Cloud Hypervisor opens or reads the kernel on restore, or reads the initramfs into guest memory on restore: the restored guest would then run a file the compatibility check does not cover.
- A later Cloud Hypervisor accepts a restore configuration that names a different payload: the binding would no longer be needed.
- `make test-cloud-hypervisor` fails its restore branch with `InitramfsFile` or with a missing `/boot/initrd`.
