# Upgrading mvmd with Machines running

The steps that replace mvmd's binaries or configuration on a host whose Machines keep running: what a restart of mvmd keeps and loses, the order, and how to go back.

Checked as `README.md` states. Every step here rests on reading the code and the architecture documents it cites, except where it says it was run.

## Contents
- What a restart keeps and what it loses
- Before you stop mvmd
- The order
- What this does not cover
- Going back

## What a restart keeps and what it loses

mvmd's Machines run as processes of their own: each VMM runs under its own `mvmd-supervisor`, which is outside mvmd's cgroup when the unit is as [install.md](./install.md) §Install the unit has it. So stopping mvmd stops neither (`docs/architecture/runtime.md` §Fencing).

| Kept | Lost |
|---|---|
| Every Running and Paused Machine, until the recorded fencing expiry | The clients' calls while mvmd is down. A `SIGTERM` waits for the calls in flight (the server's graceful stop, `daemon/internal/app/app.go:315`, read), so a long call delays it; a kill leaves the state each operation's "After failure" column names (`docs/architecture/runtime-api.md` §Operations) |
| The records, the disks, the snapshots, the images, the boot sets a client added (`state_dir`) | The clients' open streams, console sessions and exec output reads; a client opens them again |
| The fencing deadline, which keeps counting while mvmd is down | The event log, which is in memory and holds the last 1024 events (`daemon/internal/app/app.go:42`); a client that watches events starts from what Inspect and List say |
| The taps, and the nftables table `mvmd`, which mvmd replaces from its records before it serves a call | The results of probes, which the daemon reads again from each guest it follows (`docs/architecture/adr/0057-a-machine-keeps-its-probes-and-the-daemon-follows-the-agents-results-in-memory-and-streams-each-change.md`) |

The supervisor outlives the upgrade. A Machine's supervisor is the binary that started it, and it stays until the Machine is next started. How many earlier supervisors a new mvmd must speak to is not decided (`docs/architecture/runtime.md` §Open questions; [#813](https://github.com/garamsh/mvmd/issues/813)), so an upgrade that changes the conversation between mvmd and its supervisors is not covered by these steps.

## Before you stop mvmd

1. **Read the deadline.** The fencing deadline keeps counting while mvmd is down, and the client cannot renew it then. If the expiry passes in the gap, every supervisor ends its VMM (`docs/architecture/runtime-api.md` §Fencing deadline). Ask the node to Inspect the deadline, and find the time left and the last renewal number.
2. **Renew it for the gap.** Renew the deadline with a length that covers the whole stop, the start and the time mvmd takes to restore, with the margin the client keeps for its own validity, and a renewal number above the last. A Machine created `fenced` has a fence of its own, which is renewed the same way by naming the Machine, and it needs the same length (`docs/architecture/runtime-api.md` §Fencing deadline).
3. **Stop the client's calls that change state**, so that none is accepted and then cut off by the stop. gaon's Node Agent is the client: pause it for the window, and let it renew again once mvmd answers.
4. **Place the new files beside the old.** Put the new `mvmd` and `mvmd-supervisor` at new paths, and the pinned files the new configuration names at new paths, with their digests. Do not overwrite a binary a process is running: write to a new name and rename, since a running file cannot be opened for writing and a running process keeps the file it started from.
5. **Keep every name a Machine records.** A Machine records the name of its kernel and, in the OS form, of its boot set. A name the new configuration no longer offers strands the Machines that recorded it: they fail to start with the name in the error, and their records and disks are kept (`docs/architecture/adr/0045-a-node-offers-named-kernels-a-machine-records-the-name-it-is-created-with-and-a-restore-matches-the-recorded-digest.md` §Consequences). A name may point at a new file, which a Machine boots at its next start.
6. **Check the new configuration while the old mvmd runs.** Run `/opt/mvmd/mvmd -check -config <new file>` with the new binary. It loads the configuration, checks the digest of each pinned file, the kernel's Landlock ABI and `mkfs.ext4`, prints what it covered on standard error, and exits 0, or 1 with the reason. It opens no database and no socket, makes no directory and starts nothing, so it can run beside the old mvmd. It does not check the host's forwarding sysctls, the network, the cgroups or the state directory's file system, which only a start does. Keep the old configuration to go back with.

## The order

1. Stop mvmd with `SIGTERM`, as `systemctl stop mvmd` does. It stops serving, ends its streams and exits with code 0, and removes the socket (run on the `memory` Driver). The supervisors and their VMMs go on.
2. Move the `supervision.binary` and the other paths to the new files in the configuration, if they changed.
3. Start mvmd: `systemctl start mvmd`. Wait for the socket, which exists only after the restore (`daemon/internal/app/app.go:267`, read). mvmd then, in this order, ends the converter VMs a previous mvmd left, sweeps the image cache, reattaches to each supervisor, settles each Machine against the live supervisors, and compares the deadline with the clock: if the expiry has passed it stops every Machine first, and if not the remaining time is what is left of the recorded expiry, never the full length (`docs/architecture/runtime.md` §State and stability; `docs/architecture/runtime-api.md` §Fencing deadline).
4. Ask the node to list its Machines and Inspect the deadline. A Machine that was Running has its state, unless its supervisor is gone: then it is Stopped with the reason "the VMM ended while mvmd was not running" ([recovery.md](./recovery.md) §After mvmd crashes).
5. Have the client renew the deadline as it did, and let it resume its calls.

Only one process holds `mvmd.db`, so the new mvmd cannot start while the old one runs: it fails after a second with `open .../mvmd.db: timeout` (run on the `memory` Driver). Stop the old one first.

A Machine that is stopped and started again now runs under the new supervisor binary, and a Machine that is not stays on the old one. To move every Machine, the client stops and starts each, which is its decision.

## What this does not cover

- An upgrade across a change of the conversation between mvmd and its supervisors, for the reason above.
- A change to the shape of `mvmd.db` between two releases: the repository holds no migration, and whether an older mvmd reads what a newer wrote has not been tried. A record a mvmd cannot read is not discarded: the Machine is Faulted with the reason, and its stored bytes stay (`daemon/internal/machine/restore.go:22`, read).
- A change of the host's kernel or of the VMM binary under running Machines. A new VMM binary applies to the next start of each Machine, and a new host kernel needs a reboot, which is [recovery.md](./recovery.md) §After the host reboots.

## Going back

Put the old binaries and configuration back and start mvmd as above, within the same deadline. Nothing here was tried: the old mvmd has to read the records the new one wrote, and no step of this document has been run against a pair of releases.
