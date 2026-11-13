# ADR 0052: A Machine keeps a working directory, and a start from Created or Stopped may replace its env and files

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

[ADR 0051](./0051-create-machine-takes-a-command-args-env-user-and-files-which-the-machine-keeps-and-sends-at-every-boot.md) lets Create Machine set a Machine's command, args, env, user and files, and says that a change of the run configuration is a new Machine. A Secret that rotates, or a file that is renewed, then costs a new Machine, its disks and its identity. The guest agent already runs the entrypoint in the working directory a boot description names, and a Machine has no way to set it.

## Decision

`CreateMachineRequest` gains `working_dir`. It is absolute and clean, or Create refuses it as invalid, naming the path; `/` is accepted. An empty one keeps the image's working directory. The record keeps it, every boot sends it, and `Machine` reports it. A snapshot carries it and a restore puts it into the new Machine's record, as for the five fields of ADR 0051.

`StartMachineRequest` gains `run_update`, a message holding `env` and `files`. A present `run_update` replaces both lists wholesale: a list left empty clears it, and a present message with both empty clears both. An absent one keeps the record's values. The rules and bounds of Create apply to it, checked first, and a refusal leaves the record and the Machine as they were and carries no value or content. A valid update is written to the record before the boot, and the boot is made from the record. A boot that fails leaves the new values in the record, so the next Start boots with them. The record is not written when the start is refused before a boot, by the fencing deadline, the network policy or the kernel.

The command, args, user and working directory do not change at a start. A Running Machine takes no new env or files: a `run_update` equal to its record's succeeds as a repeat of the start that booted it and does nothing, and any other is refused as precondition failed, without the values.

This supersedes the sentence "A change of the run configuration is a new Machine" of ADR 0051 for env and files; it stands for command, args, user and working directory.

## Consequences

A Secret is rotated by Stop and Start with a `run_update`, without a new Machine. An update that cannot be told from a repeat of the last one is idempotent. A client that sends `run_update` to a Running Machine is told to stop it first. The new values replace the old in the database file, which only the daemon reads. A record written before this decision has no working directory and boots in the image's. The fields are additions to `v1alpha2`, so the version stays.

## Signs the decision was wrong

Clients need to change the command, args or user of an existing Machine, or to change env or files of a Running one. A failed boot that keeps values the client did not mean to keep surprises more than it helps.
