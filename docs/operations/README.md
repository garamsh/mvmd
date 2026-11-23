# Operations

What an operator does to install mvmd on a host, upgrade it while Machines run, and recover it after a failure.

Checked against commit 01ddc72743a984b10756284d9e848e08bef6f4b9 of garamsh/mvmd, by reading the code and the architecture documents it cites. The configuration example was run through mvmd's own parser, the unit file through `systemd-analyze verify`, and `mvmd` was built and started on the memory Driver. Nothing here was run against a real VMM, a real nftables table or a real cgroup tree; a step that rests on reading alone says so. A claim holds for that commit and promises nothing about later ones.

## Documents

| File | Covers |
|---|---|
| [install.md](./install.md) | What to place where, the configuration, the unit and its permissions, boot sets, the first start |
| [upgrade.md](./upgrade.md) | Replacing mvmd while Machines run: what survives a restart, the order, a rollback |
| [recovery.md](./recovery.md) | After an mvmd crash, after a host reboot, and when the state directory is damaged or lost |

These are the steps. Why mvmd behaves as it does is in `docs/architecture/`, which each step names.

## What mvmd keeps

| What | Where | Survives an mvmd restart | Survives a host reboot |
|---|---|---|---|
| Machine records, request IDs, exec records, the fencing deadline and the Machines' own fences | `mvmd.db` in the state directory | yes | yes, but the deadline and the fences are not armed |
| Root disks and disks | `disks/` | yes | yes |
| Snapshots and disk snapshots | `snapshots/`, `disk-snapshots/` | yes | yes |
| Boot sets a client added | `boot-sets/` | yes | yes |
| The image cache | `images/` | yes | yes |
| Each Machine's supervisor and VMM | processes of the host | yes, until the recorded fencing expiry | no |
| Each Machine's tap and the nftables table `mvmd` | the host's network | yes, and mvmd replaces the table from its records before it serves a call | no: a tap is made when its Machine starts |
| The event log, the results of probes | mvmd's memory | no | no |

Sources: `docs/architecture/runtime.md` §State and stability, §Fencing and §Network.

## What an operator cannot do today

Each is an issue, linked here, and none is worked around in the steps:

- There is no command line client ([#812](https://github.com/garamsh/mvmd/issues/812)). The runtime API is gRPC on a Unix socket, and its Go client is `lib/runtimeapi/`. Every step below that says "ask the node" is a call of that client.
- The supervisors are not versioned against mvmd, and how many earlier supervisors a new mvmd must speak to is an open question (`docs/architecture/runtime.md` §Open questions), so an upgrade across a change of that conversation is not covered by [upgrade.md](./upgrade.md) ([#813](https://github.com/garamsh/mvmd/issues/813)).
- Nothing backs up or restores the state directory while mvmd runs, and a lost `mvmd.db` cannot be rebuilt from the files beside it ([recovery.md](./recovery.md) §A state directory that is lost; [#814](https://github.com/garamsh/mvmd/issues/814)).
