# ADR 0070: mvmd speaks the supervisor API of its own release, and refuses to start beside a supervisor that speaks another

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A supervisor is the binary that started its Machine, and it outlives an mvmd upgrade until the Machine is next started ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)). So a new mvmd finds supervisors of earlier releases, and how many earlier releases it must speak to was an open question (garamsh/mvmd#813). The supervisor already reports the API it speaks in Inspect, `api_version`, and `v1alpha1` is the only one that has existed. A release before the field existed reports none.

## Decision

An mvmd speaks the supervisor API of its own release and the earlier releases' that reported none, and refuses to start beside a supervisor that reports another.

- **What is spoken.** The version the release's supervisor reports, today `v1alpha1`, and an absent version, which stands for the releases that did not report one: they spoke `v1alpha1`, as the proto file states for every later release ("a field is only ever added, and a reader treats an absent field as unknown"). So within a version every earlier supervisor is spoken to, and a new version is a new ADR that says which earlier ones its mvmd speaks to.
- **The refusal.** At its start, before it ends a converter, sweeps the image cache, restores a Machine or serves a call, mvmd asks each supervisor it finds for its version. For the first, by Machine identifier, that reports a version it does not speak, `Serve` returns an error that names the Machine and the version, and nothing has been changed: no supervisor was passed an expiry, ended or listed as unclaimed, and no record was written. The operator stops that Machine with the mvmd that started it, or waits for its fencing expiry, and starts again.
- **A supervisor that does not answer** reports no version, so it is spoken to as it is today: the listing carries it with the earliest expiry, and the caller passes it its expiry again.
- **A listing that fails** is no list: the stage checks what it can list and goes on, and the restore and the deadline's reconciliation list again and fault as they did before this decision.
- **The supervisors of conversions** are not checked: a conversion's supervisor lives for at most the conversion's time limit, and the start ends it before it is used.
- **No translation.** mvmd does not speak two versions at once. A release that changes the conversation incompatibly says so in a new version and an ADR.

## Consequences

- An upgrade across a change of the supervisor API is refused at the new mvmd's start and changes nothing, and the Machines run on under the old mvmd's supervisors until their expiry. It no longer risks a half-spoken conversation.
- A node whose supervisors all speak the current version starts as before.
- The refusal can hold a node down until the operator ends the incompatible Machine, and the Machines of every other supervisor wait with it, for their expiry runs on while mvmd is down.

## Signs the decision was wrong

- An incompatible supervisor is common enough that one Machine's version holds a whole node down, and mvmd should settle that Machine on its own and start.
- A version of the supervisor API is added that is compatible with an earlier one, and a refusal on a different name is wrong.
