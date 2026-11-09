# ADR 0023: A restart settles a Running Machine's disks against the VMM's drives

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#125, from [ADR 0022](./0022-the-guest-finds-a-hot-plugged-disk-by-rescan-and-releases-it-before-the-unplug.md). A hot attach on a Running Machine is three steps in order: the record lists the disk, the Driver puts it on the VMM, the guest finds it. The daemon can die after any of them. After a restart the record says the disk is attached, and a repeated attach answered from the record says so too, though the VMM or the guest may lack it. The Machine would report a disk as attached and healthy that the guest cannot use.

Forces:

- Restore runs once before any call, with the supervisors and VMMs alive ([runtime.md](../runtime.md) §State and stability), so the VMM can be asked what drives it has.
- The record's stored form is the State Store adapter's, and a marker for an attach in progress would change it for every Machine.
- Firecracker names a disk's drive by the hex of the SHA-256 of the disk's ID ([driver.md](../driver.md) §Disk attach and detach), so a list of the VMM's drives cannot be turned back into disk IDs. The question has to be asked of one disk at a time.
- A disk the Machine booted with is not known to the guest ([ADR 0022](./0022-the-guest-finds-a-hot-plugged-disk-by-rescan-and-releases-it-before-the-unplug.md)), so asking the guest to find it fails. The answer to "does the VMM have a drive for it" is not enough: how the VMM came by the drive decides whether the guest is asked.
- The guest may be using a disk the VMM has. Only the guest's release makes an unplug safe, and a restart has no request to release it.
- A Paused Machine cannot be attached to ([ADR 0022](./0022-the-guest-finds-a-hot-plugged-disk-by-rescan-and-releases-it-before-the-unplug.md)), so it cannot hold a half-done attach.

Candidates weighed:

- **Ask the VMM, one disk at a time, and settle the record to what it says** — chosen.
- **A pending marker in the record.** It would tell a half-done attach from a finished one without asking the VMM, and it changes the stored form of every record.
- **Listing the VMM's drives.** Not possible for Firecracker, since the drive's name cannot be reversed to a disk ID.
- **Undoing a half-done attach at a restart by unplugging.** The guest may be using the disk, so the unplug is not safe without the guest's release, and the restart does not know the guest's state.
- **Leaving the record as it is and Faulting nothing.** The Machine would report a disk attached that is not.

## Decision

- **The Driver port gains `Drive`.** For one Machine and one disk it reports whether the running VMM has a drive for it: absent, given at boot, or added since boot. It reads the VMM and changes nothing, and needs the capability `disk-hotplug` like attach and detach. The Firecracker Driver reads `GET /vm/config` and tells an added drive by its path in the run directory.
- **Restore settles each disk a Running Machine's record lists, after it has settled the Machine against its supervisor.**
  - A drive the VMM does not have: the attach never reached the VMM, or the unplug finished before its record did. The record drops the disk and the holder is released, so the disk can be attached again.
  - A drive added since boot: the guest finds the disk again, which is repeatable. If the guest cannot, the Machine is Faulted with a reason that names the disk.
  - A drive given at boot: nothing is asked of the guest, which does not know it.
  - A VMM whose drives cannot be read: the Machine is Faulted with a reason that names the disk.
  - Nothing is unplugged. A Machine that is not Running is not settled: a Paused one cannot hold a half-done attach, and one without a VMM has no drives.
- **A repeated attach of a disk the record lists on a Running Machine asks the same question.** It succeeds when the VMM has the drive and the guest has found it, finds the disk again when the VMM has it and the guest may not, and attaches it as the first call would when the VMM does not have it. A guest that cannot find a disk the VMM has fails the call, and the disk stays attached and recorded, since the guest may be using it. A Machine that is not Running, or a Driver without `disk-hotplug`, has no drive to ask about and the call answers from the record as before.

## Consequences

- After a restart, a Running Machine lists only disks its VMM has, and a disk the guest could not be made to find is a Faulted Machine that names it. No record is changed in form.
- The Driver port grows by one operation that every implementation of it serves.
- A restart now asks every Running Machine's VMM for its drives, once per disk the record lists, which delays Restore by those calls.
- A repeated attach on a Running Machine now calls the Driver and the guest, where it called neither.
- A detach whose release reached the guest but whose unplug did not leaves a drive the guest no longer has. The restart has the guest find the disk again, so the Machine stays Running with the disk attached, and a detach called again finishes it.
- An attach the daemon undid by detaching leaves the link of the disk in the run directory when the detach died before removing it. The next attach of that disk removes it. Nothing else reads it.

## Signs the decision was wrong

- A restart Faults Machines whose disks the guest could not find for a reason a second try would have removed: the settle should retry.
- A real VMM's `GET /vm/config` omits a drive that was hot-plugged, or reports its path other than the one the Driver sent, so an attached disk reads as absent and its record is dropped. The real test reads it (`make test-firecracker`).
- A State Store change that adds a field to the record, which would let the attach be marked and the VMM left unasked.
- A VMM Driver that can list its drives by disk ID, which would let one call replace the per-disk question.
