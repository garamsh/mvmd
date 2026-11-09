# ADR 0028: FindDisk takes the one virtio block device the agent has not accounted for

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#178, from garamsh/mvmd#177 (garamsh/mvmd#142), which gives the Cloud Hypervisor Driver disk hot-plug. Its test `TestReal_AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug` failed with `find the disk: no new virtio block device appeared after the rescan` (run 42712121030).

- [ADR 0022](./0022-the-guest-finds-a-hot-plugged-disk-by-rescan-and-releases-it-before-the-unplug.md) has `FindDisk` list the virtio block devices just before its rescan and wait for exactly one that is not on that list. It rested on Firecracker v1.17.0 delivering no hotplug notification, so only the agent's rescan adds the device.
- Cloud Hypervisor v54.0 sends an ACPI hotplug notification on `vm.add-disk`. The guest kernel probes `0000:00:05.0` and adds `vdc` on its own. A device the kernel has already added is on the listing taken before the rescan, so `FindDisk` never sees it as new.
- ADR 0022 names this case among its signs the decision was wrong: a later VMM Driver begins delivering a hotplug notification and the kernel's own handling replaces the rescan.
- The order of the kernel's own add and the agent's rescan depends on the VMM, and on one VMM it can vary: the device can be there before the call, appear after the rescan, or appear while the call waits.
- Calls for one Machine are serialised, so one attach cannot take another's device.

Candidates weighed:

- **The one virtio block device the agent has not accounted for** — chosen. Accounted for means present when the agent started, or held by a record for a disk ID.
- **Telling Cloud Hypervisor not to notify.** Rejected: it works around the VMM instead of meeting it, and the next VMM brings the same case.
- **Identifying the device by a virtio serial or a PCI address the Driver reports.** Rejected: Firecracker v1.17.0 offers neither for a drive (ADR 0022's finding), so the contract would gain a field one of its two VMMs cannot fill. It stays the route if garamsh/discussions#2 §6 item 9 (identity for a non-root disk) is decided, as ADR 0022's last sign already says.
- **Listing the devices before the rescan, as ADR 0022 did.** Rejected: it fails for a VMM that notifies, as above.

## Decision

- **The agent records the virtio block devices present when it starts as accounted for.** The record is by device name with its PCI address, and it is taken once, before the agent listens.
- **`FindDisk` takes the one virtio block device that is neither among those nor held by a record.** It still writes `1` to `/sys/bus/pci/rescan`, so a VMM that sends no notification still shows the device, and then waits up to the existing bound for exactly one unaccounted device, whichever order the kernel's own add and the rescan came in.
- **Two or more unaccounted devices, or none within the bound, fail the call and record nothing,** as in ADR 0022.
- **A released record's device is accounted for while it is present.** `ReleaseDisk` marks a record released only once the device is gone, so the case does not arise: a released record holds nothing, and a device that later appears under the same name at the same address is a new one.
- **Everything else in ADR 0022 stands:** `ReleaseDisk`, the daemon's ordering around the Driver's calls, the fail-closed detach, the refusal while Paused, the serialised calls, and the repeat of `FindDisk` for a held ID returning its record. This ADR supersedes only ADR 0022's rule for which device `FindDisk` takes.

## Consequences

- One rule serves a VMM that notifies the guest and one that does not, with no knowledge in the agent of which it runs on.
- The identification is by exclusion, so it is sound only while every device present at the agent's start is a boot device and the calls for a Machine are serialised. A boot device the kernel adds after the agent has started listing would be taken for the disk, or make two unaccounted devices and fail the call.
- A virtio block device that appears for another reason while a call waits is taken for the disk, as under ADR 0022.
- The agent holds one more map for the life of the guest, and a record now also keeps a device from being taken for another disk's.

## Signs the decision was wrong

- A real guest, on a node or in a real test, finds a boot device unaccounted for because the kernel added it after the agent took its listing, so a `FindDisk` fails with two unaccounted devices or takes a boot device.
- A VMM adds a virtio block device to the guest that is not a hot-plugged disk, between the agent's start and an attach.
- A VMM or kernel reuses a device name and PCI address for a different disk while the first is still recorded as held.
- Item 9 is decided with an identity for a non-root disk (a UUID, a name, a serial or a PCI address the Driver can report for both VMMs), which would let the agent identify the device by it and drop the exclusion rule.
