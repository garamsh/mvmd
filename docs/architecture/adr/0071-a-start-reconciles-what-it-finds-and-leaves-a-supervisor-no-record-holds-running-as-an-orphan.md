# ADR 0071: a start reconciles what it finds, and leaves a supervisor no record holds running as an orphan

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

At its start mvmd lists the supervisors it finds and settles each against the Machine records ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md), [ADR 0033](./0033-a-vmm-that-ends-on-its-own-moves-its-machine-to-stopped-with-the-reason.md)). A supervisor that no Running or Paused record claims was terminated, whether a record held its Machine or none did. When `mvmd.db` was lost, empty or replaced, every supervisor was of the second kind, so the first start ended every running Machine, though their disks were still files in `disks/` (garamsh/mvmd#814). Nothing backed the database up, so such a loss was not rare in principle.

## Decision

A start reconciles what it finds, in one stage whose cases are told apart by what the records say. The stage is the same for every supervisor, and the unrecorded one is one of its outcomes and not a special case.

- **A supervisor a Running or Paused record claims** is kept, as before.
- **A supervisor a record holds, whose state is not Running or Paused**, was started by a call that never reported success, and is ended, as before.
- **A supervisor no record holds** is an orphan. mvmd leaves it running and untouched, makes no record of it, and reports it: a line in the log, `orphaned_supervisors` in runtime info, and an event of the Machine's identifier, `SupervisorOrphaned`, once for each at the start. The operator decides what it is.
- **What mvmd keeps doing for an orphan.** The fencing deadline applies to it as to every supervisor: it is passed the node's expiry, and it ends at the expiry. Its state directory is not swept while it runs. It leaves the list when its supervisor has ended, which the daemon sees in its look at the supervisors every second; the next start sweeps the state directory it left.
- **The orphan's expiry.** After a lost `mvmd.db` the fencing deadline record is lost too, so the node's deadline is not armed at that start. The orphan keeps the expiry it last held, which its own supervisor counts, and it is passed the node's expiry by the first renewal of the deadline after the start, as every supervisor is. A node whose client does not renew leaves the orphan to the expiry it had.
- **An orphan's identifier is not made.** Create and Restore Machine refuse an identifier an orphan holds, as precondition failed naming it, before they take or record anything: a record of it would claim a supervisor no call started, and the next start would end it. The identifier is free once the supervisor has ended.
- **What mvmd does not do.** It adopts nothing and rebuilds no record. No call names an orphan, and no call ends one: the operator ends it by ending its supervisor on the host, or waits for its expiry.
- **The records are backed up with mvmd stopped.** `mvmd -backup <file> -config <file>` writes a consistent copy of `mvmd.db`, read-only, to a file that does not exist, and fails while another process holds the database. A running mvmd holds it, so the copy is made in the window an upgrade already keeps for a stop ([docs/operations/upgrade.md](../../operations/upgrade.md)). Putting a copy back is a file copy before the start.

## Consequences

- A lost or empty `mvmd.db` no longer ends the running Machines at the next start: they run on, listed as orphans, until the operator ends them or their expiry passes.
- An orphan has no network. The network reconciliation deletes every tap that belongs to no Machine of the records, and replaces the table from them, so an orphan's VMM runs on without its tap.
- A Machine identifier an orphan holds cannot be created: its state directory is in use, and the Create fails there.
- A copy of `mvmd.db` taken earlier than the files beside it is not known to be consistent with them: its records may name a state the disks and supervisors have left. That case was not tried.

## Signs the decision was wrong

- Operators need to end or adopt an orphan from the API, and ending its supervisor on the host is not an acceptable step.
- A recorded Machine's supervisor that no Running or Paused record claims needs the same leave-and-report treatment.
