# ADR 0057: A Machine keeps its probes, and the daemon follows the agent's results in memory and streams each change

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The Guest Agent runs a workload's probes and streams their results with `WatchProbes`, and acts on none ([ADR 0054](./0054-the-guest-agent-runs-the-workloads-probes-and-streams-their-results-and-acts-on-none.md)). A client of the runtime API had no way to give a Machine probes or to read what they found. What is open is where the probes live, who opens the stream and when, what is kept, what an event says, and what a Machine that is not running reports (#438).

## Decision

**The probes are the Machine's.** `CreateMachineRequest` gains `probes`, and Create validates them with the agent's rules (ADR 0054) and refuses what the agent would refuse, as invalid, naming the probe. `probes` joins Create's repeat key, in order, so the same request is the same call and other probes under the same Machine identifier are a conflict. The Machine record, its bolt store entry, a snapshot's record and a restore keep them as they keep `working_dir` ([ADR 0052](./0052-a-machine-keeps-a-working-directory-and-a-start-from-created-or-stopped-may-replace-its-env-and-files.md)); a run update does not change them, and every boot sends the guest the record's.

**The daemon follows.** It opens `WatchProbes` once a Machine with probes is Running: after Boot, after a restore's catch-up, and for each Running Machine when the daemon starts again. The guest sends the current results on open, so a restart loses nothing. A stream that breaks is opened again after a short wait; a stream the agent ends cleanly, which it does after the entrypoint ends, is not. The follower is kept while the Machine is Paused and ended in any other state.

**Results are in memory only.** A result is a reading, not a record: it is rewritten by the guest at every change, and a write for each would load the store for nothing. After a restart the daemon reads them back from the guest.

**One event kind, on a change.** `WatchEventsResponse` gains `probe_changed`, naming the Machine through the envelope and carrying the probe's new result. It is emitted only when the result differs from the one the daemon held, so a stream that is opened again and sends what the daemon holds emits nothing, and no event follows the state change that ended the following.

**What a state reports.** Inspect and List report one result for each probe, in the order of the request. A Running Machine reports what the guest last sent, a Paused one the results held, and a Machine in any other state reports `UNKNOWN` for each probe with no per-probe event, since the state's own event says why.

`tail_bytes` on Read exec output is not part of this decision. It needs an end offset that the guest's `ExecRecord` does not carry, and it is decided in #618 (guest) and #619 (go).

## Consequences

A client reads a workload's health with one Inspect or one stream and needs no call into the guest. Nothing in mvmd acts on a result: restarting, routing and fencing on it stay the client's. The node holds one goroutine and one stream for each Running Machine that has probes. A result a daemon restart loses is read back, and a transition that happened while the daemon was down is seen as a change from `UNKNOWN`.

## Signs the decision was wrong

Clients need the history of a probe, not its latest result. Clients need results of a Machine that is Stopped. A node holds so many probed Machines that a stream for each costs more than polling. Clients need mvmd to act on a result.

## Errata

- 2026-11-13. The decision stands. §Decision's last paragraph says `tail_bytes` on Read exec output needs an end offset that the guest's `ExecRecord` does not carry. It does not need one: the Guest Agent resolves the tail against the output it holds when the call arrives (`ReadExecOutputRequest.tail_bytes`, commit df306f633d99575a88003478987255db3cfc5d80), and mvmd passes the field on. `docs/architecture/runtime-api.md` §Guest access states the field, decided in #619.
