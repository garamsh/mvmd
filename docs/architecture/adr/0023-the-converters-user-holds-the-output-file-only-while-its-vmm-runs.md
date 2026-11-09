# ADR 0023: The converter's user holds the output file only while its VMM runs

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#131, found by the `e2e` job of garamsh/mvmd#118: every pull on a real node failed at the converter's boot with `firecracker refused PUT /drives/disk0: 400 ... Error manipulating the backing file: Permission denied (os error 13) /disk/0`. This ADR adds to [ADR 0007](./0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md) step 6 and step 7, whose bodies stay as they are.

What happens today, in `daemon/` at commit fa3372ca521e9784dc95ff95e123a1dff7452d87:

- `Store.Stage` creates the staging area's `output` file with mode 0600, owned by the daemon, which runs as root.
- `convert` allocates the converter's user from the node's UID range after the stage exists, and passes `output` as the VMM's only device. The converter's VMM runs as that user ([ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md)).
- The supervisor bind-mounts the file into the VMM's root as it is ([ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md)), so its owner and mode reach the VMM unchanged. The user cannot open a file that is root's and mode 0600.
- A Machine's root disk is given to the Machine's user when it is created (`DiskOwner`, [ADR 0009](./0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)) and stays its until the Machine is deleted. A conversion's output has no counterpart, and unlike a root disk it is read afterwards by the daemon: truncated, hashed and sealed with fs-verity ([ADR 0008](./0008-fs-verity-on-the-host-guards-every-read-of-a-machineimage.md)), then published. The content is untrusted (`image.md` §Conversion).

What a user can do with a file it owns: read and write it, change its mode, and, by keeping an open descriptor, go on writing after the owner changed. A user is reused by the next conversion once it is released.

Candidates weighed:

- **The daemon gives the output to the converter's user before the VMM starts and takes it back after the VMM has ended, before it reads or seals it** — chosen.
- **Give it away for good, as a root disk is.** The daemon would then hash, seal and publish a file whose owner could change it, and a user reused by a later conversion would hold a file of an image already published. fs-verity does not help: it is enabled after the hash is taken, and a writer holding the file open until then changes what was hashed.
- **Open the file in the daemon and pass its descriptor to the VMM.** The VMM opens the path it is given, at the fixed name ADR 0019 sets, so a descriptor would change the Driver and Supervision ports for every Machine to serve one caller.
- **Loosen the mode** (0666, or a group the converter's users share). Any unprivileged user, or any converter, could then open another conversion's output.
- **Run the converter's VMM as root.** Ruled out by ADR 0015.
- **Create the output owned by the user in `Stage`.** The user is allocated after the stage, and the stage is also where the output stops being the daemon's, so the taking back would still be needed; it would move the allocation before the stage and make `Stage` know about the allocator.

## Decision

The Image Store port the pull uses gains two calls, `Lend(staging, user)` and `Reclaim(staging)`. `Lend` gives the staging area's output to the converter's user, keeping its mode (0600, so the owner alone can open it). `Reclaim` gives it back to the daemon and sets the mode again. A pull lends before it describes or starts the converter's VMM, and reclaims after the supervisor has ended the VMM, before it reads the converter's report to truncate and seal.

- **On every path** that ended the VMM, success or failure, `Reclaim` runs once, whether or not the VMM booted.
- **When the VMM cannot be ended**, the output is not reclaimed: a VMM that is still running holds the file whatever its owner. The pull fails and the user stays held until mvmd restarts, as `image.md` already says of a VMM that cannot be ended.
- **When `Reclaim` fails**, the pull fails and the user stays held for the same reason: a reused user would own a file the daemon has not got back.
- **When `Lend` fails**, no VMM was started, and the user is released.
- **`Seal` refuses an output the daemon does not own**, so a path that skipped the taking back fails and does not publish.

## Consequences

- The converter's user can open the output while its VMM runs and no other unprivileged user can, since the mode stays 0600 and the staging area's directory stays the daemon's alone.
- After the VMM ends, a descriptor the converter held is dead with its process, the owner is the daemon's and the mode is the one the daemon set, so nothing the converter's user or a reuse of it does changes the image the daemon is checking.
- The Image Store port has two calls more, and the in-memory substitute and the adapter implement them; the Driver and Supervision ports are unchanged.
- A pull that cannot end its VMM or take back its output leaks one user of the UID range until mvmd restarts, as before for the VMM.
- A restart of mvmd leaves a staging area whose output belongs to a user; the start-up sweep removes the area, which needs no ownership of the file.

## Signs the decision was wrong

- A pull fails at `Reclaim` on a host where the daemon holds the privilege to give files away, so the call is not reliable.
- A node's UID range runs out because users held after a failed `Reclaim` or `Terminate` accumulate.
- A second VMM, a Driver other than Firecracker, needs the output through a different path, and the lending has to be reworked per Driver.
