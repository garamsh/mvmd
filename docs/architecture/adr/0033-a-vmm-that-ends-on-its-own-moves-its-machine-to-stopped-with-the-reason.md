# ADR 0033: A VMM that ends on its own moves its Machine to Stopped with the reason

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#223. A Machine whose entrypoint ends powers its guest off ([guest.md](../guest.md) §PID 1 duties), and a guest panic or an agent failure ends the VMM as well. What happens today, in `daemon/` at commit 54b5f4b:

- Nothing in the daemon looks at a Running Machine's VMM between calls. A Machine whose VMM has ended stays Running in the record until the next call on it fails, or until mvmd restarts and its reconciliation finds no supervisor ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md): the supervisor exits with its VMM).
- The reconciliation gives such a Machine Stopped with the reason "the VMM ended while mvmd was not running", with no cause.
- The supervisor role's contract has no call that reports an end; `Supervision.Live` lists the Machines whose supervisor exists, and the stop's wait for the VMM's end already uses it.

Candidates weighed for how the daemon learns of the end:

- **List the supervisors on an interval** through `Supervision.Live`, which exists — chosen. It needs nothing of the supervisor and nothing of the guest.
- **A notification from the supervisor** when its VMM ends. It changes the supervisor's contract and its paths for a latency of at most the interval.
- **Ask the guest.** A guest that has ended cannot answer, and a polled guest is the cost the issue rules out.

## Decision

A Running or Paused Machine whose supervisor is gone, and which no lifecycle call holds, moves to Stopped, the state Stop reaches: disks retained, the network withdrawn as Stop withdraws it. An event is emitted for the transition.

- **When it is looked for.** The daemon lists the supervisors every second while it serves, after the start-up reconciliation, which gives the same transition to a Machine that ended while mvmd was down.
- **The reason** begins "the VMM ended with no call asking it to" (at start-up, "the VMM ended while mvmd was not running") and names the cause from the Guest Agent's last console line: the entrypoint's exit code or signal, an entrypoint that did not start, or the agent's failure or panic. A VMM that ended with no such line is reported as that. The entrypoint's record is read afterwards from the console log, as for any Stopped Machine.
- **A call that holds the Machine owns it.** The look takes the Machine's lock without waiting and leaves a Machine a call holds, then establishes the end again under the lock, since its listing is older than the lock. A Stop, Snapshot, Restore or Delete that the VMM's end overtakes ends as the call's outcome says, with no reason of the VMM's own and one event.
- **A VMM end mvmd cannot establish is Faulted**: the network that could not be removed leaves the Machine Faulted, as in a Stop. A listing that fails observes nothing and is tried again at the next look.
- **Restart policy is not mvmd's.** mvmd reports the end; whoever drives mvmd decides whether the Machine is started again.

## Consequences

- A client learns of a Machine's end within a second of the supervisor's, from an event and from Inspect, instead of at its next failed call.
- A Paused Machine whose VMM ended is Stopped too.
- No supervisor-role path changes.
- A VMM that ends and a Machine started again within one interval is not seen as ended, since the supervisor exists again; the Start's own record holds.
- The reason is only as good as the console log: a log the Driver cannot read gives the reason that says so.

## Signs the decision was wrong

- The interval is too coarse for a client that needs the end at once, which would justify a notification in the supervisor's contract.
- Listing every supervisor each second costs enough on a node with many Machines to matter.
- A listing that fails for a long time hides ends that the Machine's own calls would have found.
