# ADR 0051: Create Machine takes a command, args, env, user and files, which the Machine keeps and sends at every boot

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The guest agent runs an entrypoint as a named user and places files from the Boot call ([ADR 0048](./0048-the-guest-agent-runs-the-entrypoint-as-a-named-user-and-places-files-from-the-boot-call-in-a-tmpfs-it-alone-mounts.md)), and an image's entrypoint, command and user are kept apart in its record. Nothing yet lets a client set any of them for one Machine. A client needs to run an image with another command, with an environment that carries a Secret, as a chosen user, and with files that are not in the image.

## Decision

`CreateMachineRequest` gains `command`, `args`, `env`, `user` and `files`. They follow Kubernetes: a `command` replaces the image's entrypoint and drops its command unless `args` is set, `args` alone replaces the command and keeps the entrypoint, and neither keeps both. `args` alone on an image record that holds one joined argument vector, which has no command to replace, is refused as precondition failed, naming the image and saying to pull it again. `env` is applied over the image's environment. An empty `user` applies the image's user, and root where the image sets none.

Create refuses, as invalid and naming the field and the name or path, an env name that is empty, contains `=` or is given twice, and a set of files with a path that is not absolute and clean, is `/` or is under `/proc`, `/sys`, `/dev` or `/run/mvmd`, a mode above 07777, a path given twice, more than 64 files, or more than 1 MiB of content in all. These are the limits the guest holds to, checked first so that a bad request never reaches a boot. A message never contains an env value or a file's content.

The Machine's record keeps all five fields, and every Start sends the guest the same run configuration: the image's, merged with the record's, is computed at each Start from the image the Machine names. Env values and file content are held in the daemon's database file, which only the daemon's user reads.

`Machine` reports the env names and, for each file, its path, mode, uid, gid and size, and nothing else. No call, event, error or log carries a value or a content.

## Consequences

A Secret is in the database file at rest, in the daemon's memory for as long as the record is held, and in guest memory only. A client that cannot accept the first holds the Secret itself and sends it at Create. A record written before this decision has none of the fields and boots with the image's own. A Machine restored from a snapshot is a new record whose request carries none of them, so a restored Machine that is later stopped and started boots with the image's run configuration; the fields are not part of a snapshot. Inspect cannot show a changed env value. A change of the run configuration is a new Machine. The fields are additions to `v1alpha2`, so the version stays.

## Signs the decision was wrong

Clients need to change the run configuration of a Machine that exists, or to restore a Machine with the configuration it was snapshotted with. A Secret must not be in the database file. The memory held by a record's files, up to 1 MiB in all, shows in a node that holds many Machines.
