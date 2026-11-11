# ADR 0042: The supervisor links the files a process description lists into the run directory before the VMM starts

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#332 from the finding in garamsh/mvmd#326. [ADR 0041](./0041-a-snapshot-records-its-disks-a-restore-takes-exactly-those-and-a-non-root-disk-reaches-the-guest-by-hot-plug.md) decision 3 has the Driver hard link each recorded disk into the run directory before it asks the VMM to load the snapshot. That cannot be done on Cloud Hypervisor:

- Its VMM loads the snapshot from its command line (`--restore`), so the load happens as the process starts.
- The supervisor creates the run directory and gives it to the Machine's user inside `Supervision.Start`, and `Start` goes on to start the VMM. No Driver call runs between those two steps (`daemon/internal/supervision/supervision.go`, `daemon/internal/machine/boot_pipeline.go`).
- `DescribeRestore` runs before the run directory exists and describes a process without changing anything, so it cannot link either.

Firecracker loads over its API after the process is up, and could link first, but two mechanisms for one need would split the Drivers. The PM decided on 2026-11-11, inside the approved goal of #326, that the description names the files and the supervisor links them.

## Decision

1. **The process description carries `RunFiles`: for each file, a name in the run directory and an absolute host path.** It is part of the VMM-neutral description the Driver states ([ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md)); it grants nothing the VMM did not already have, since the run directory is where mvmd adds files to a VMM ([ADR 0041](./0041-a-snapshot-records-its-disks-a-restore-takes-exactly-those-and-a-non-root-disk-reaches-the-guest-by-hot-plug.md) names it so).
2. **`Supervision.Start` hard links each file after it creates the run directory and gives it to the Machine's user, and before it starts the VMM.** It refuses a name that is empty, `.` or `..`, or contains `/`, and a path that is not absolute, before it creates or links anything. A link that fails fails `Start` with the file's name in the reason, and no VMM is started. A hard link only: no copy and no bind mount, so the file and its link are one inode and the check of [ADR 0041](./0041-a-snapshot-records-its-disks-a-restore-takes-exactly-those-and-a-non-root-disk-reaches-the-guest-by-hot-plug.md) decision 4 sees what the guest writes.
3. **A restore's disks reach the VMM this way on both Drivers.** `DescribeRestore` lists each recorded disk in `RunFiles` under the name its ID derives (`daemon/internal/diskdevice/`) and the path of its file. It stays a description with no side effect, and neither Driver links a file itself on restore. This replaces [ADR 0041](./0041-a-snapshot-records-its-disks-a-restore-takes-exactly-those-and-a-non-root-disk-reaches-the-guest-by-hot-plug.md) decision 3, the one decision of that ADR this ADR supersedes; its decisions 1, 2 and 4 to 6 stand, as does its candidate "a link in the run directory from the ID", which now names the supervisor as the one that makes it.

`Attach` on a running VMM is unchanged: the Driver links the file, since the supervisor has already started the VMM and holds no call for it.

## Consequences

- One mechanism puts a disk where a restored VMM's saved state names it, so neither Driver reads the snapshot for a path or keeps a record of one.
- A file the supervisor links is a host file named by a Driver, so `Start` checks the name and the path as the boundary it is, and a Driver cannot name a link outside the run directory.
- The disk's file and the run directory must be on one file system, as for an attach. A link across file systems fails `Start`, and the reason names the file.
- The run directory outlives the VMM, so a name may already hold a leftover, such as the link of an attach a Stop left. `Start` removes what stands at the name without following it and then links, as an attach does, and a removal that fails fails `Start` with the file's name in the reason.
- A hard link has the file's owner and mode, so the VMM's user reads and writes a disk through its link as [ADR 0038](./0038-a-disk-file-belongs-to-the-machine-that-holds-it.md) lets it the file.

Candidates weighed:

- **How the files reach the run directory before the VMM loads.**
  - **The description lists them and the supervisor links them** — chosen. It is the one step that runs between the directory's creation and the VMM's exec, on both Drivers.
  - **The Driver links in `Restore`**, as ADR 0041 decision 3 says. Cloud Hypervisor's `Restore` runs after the process started with `--restore`, so the load has already failed.
  - **A new Driver call between the run directory and the exec.** It adds a step to the lifecycle for one need, and a Driver call with a side effect on the host.
  - **`DescribeRestore` links.** It would make a description a side effect and link into a directory that does not exist yet.
  - **Bind each disk into the VMM's root.** ADR 0041 rejected it: the root is fixed when the supervisor starts the VMM, and a name would be a Driver-specific record.
  - **Copy the file.** The disk is the client's data and has one holder ([ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)), and the copy would not see the guest's writes the check watches.

## Signs the decision was wrong

- A VMM that needs a file at a path a link cannot give it, such as one on another file system than the disk.
- A need to link after the VMM starts, which the supervisor has no call for.
- A Driver that lists a name that is not a file name and is refused, which would mean the rule for the name belongs in the description's type.
