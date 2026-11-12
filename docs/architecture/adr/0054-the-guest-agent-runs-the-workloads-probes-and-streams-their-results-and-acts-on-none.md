# ADR 0054: The guest agent runs the workload's probes and streams their results, and acts on none

> Status: accepted
> Date: 2026-11-13

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

A Machine's workload can be running and still not be serving. Only the agent sees the inside of the guest: a command run there, a port opened on its loopback. A client that wants to know whether the workload is ready or has stopped answering has no call to ask, and the agent is where such a check has to run.

## Decision

`RunConfig.probes` lists up to 8 probes, each with a unique name, a kind (`exec` with an argument vector, `tcp` with a port, `http_get` with a port, a path and the scheme `http`), an initial delay, a period, a timeout, a success threshold and a failure threshold. An unset value takes the default: period 10 s, timeout 1 s, success threshold 1, failure threshold 3, initial delay 0. The defaults are applied at the parse, so the description a repeat `Boot` is compared by holds the effective values, and probes are part of it.

Each probe starts its initial delay when the entrypoint starts and runs every period until the entrypoint ends. A probe is UNKNOWN until its first threshold is met, then PASSING or FAILING. `WatchProbes` is a server stream of the current results on open, then one message for each change of a status, ending after the entrypoint has ended and the final results are sent. The agent acts on no result.

The issue left these choices, and they are made here. A duration is at most 24 h, and a period or timeout at least 1 ms, so that a value cannot overflow a timer or spin the agent. An exec probe takes the run configuration's environment, working directory and user, since it is meant to check what the entrypoint sees, and is killed at its timeout and when the entrypoint ends. A message is sent when a status changes and not when a count does, so a stream is quiet while a probe is steady. A watcher that falls behind the buffer of 64 messages is sent the current result of every probe in place of the changes it missed, because the current state is what it needs and a stream that dropped a message silently would be wrong. `tcp` and `http_get` connect to `::1` when the network configuration has IPv6 and no IPv4, and to 127.0.0.1 otherwise. The HTTP check is a minimal client in the agent that reads the status line only and succeeds on 200 to 399, so that the agent takes no HTTP dependency and a probe cannot be made to read a large body. The failure message names the cause and never holds output of the workload; an exec probe's output is kept apart, as its first 1 KiB.

The change is additive to `v1alpha1`.

## Consequences

A client can tell a running workload from a serving one, and the daemon can build readiness and restart policy on the stream without the agent holding any. A Machine with no probes is unchanged. A repeat `Boot` with other probes is refused, as one with another command is. The agent spends a task for each probe and a process for each exec run while the entrypoint runs. A probe that reads the status line only cannot check a response's body or headers, and `https` is not offered.

## Signs the decision was wrong

Clients need a check the three kinds cannot make, such as an HTTP header or body, TLS or a gRPC health check. Clients need the agent to act on a result, because a round trip to the daemon is too slow. Watchers that fall behind are common enough that the resync replaces most of the stream. The exec probes' cost in a Machine with many probes shows.
