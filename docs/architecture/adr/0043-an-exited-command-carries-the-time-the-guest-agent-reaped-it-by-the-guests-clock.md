# ADR 0043: An exited command carries the time the Guest Agent reaped it, by the guest's clock

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#331. An exec record said that a command exited and with what, and not when. A client that reads the record after the fact, which is what resumable output and a Machine that ended on its own both lead to, cannot tell how long ago the command ended. The entrypoint's record of a Machine whose VMM has ended comes from a line in the console log ([guest.md](../guest.md) §PID 1 duties), which had no time either.

Only the Guest Agent sees the moment: it reaps the command. The node reading the record later, or taking the time at which it learned of the end, would give a time that depends on when a client or the daemon looked, not on when the command ended.

## Decision

1. **`ExecRecord` has `google.protobuf.Timestamp ended_at`** in the guest contract (`mvmd.guest.v1alpha1`) and in the runtime API (`mvmd.runtime.v1alpha2`). It is set when the status is `EXITED`, to the guest's `CLOCK_REALTIME` at the moment the agent reaps the command, and unset otherwise. It covers every exec and the entrypoint. `Machine` has no new field. Both are additive, so neither version changes ([ADR 0014](./0014-the-guest-agent-serves-a-grpc-contract-it-owns-over-vsock.md)).
2. **The console lines `entrypoint exited code=<n>` and `entrypoint exited signal=<n>` end with ` at=<time>`**, RFC 3339 in UTC, as `prost_types::Timestamp` writes it: a fraction of 0, 3, 6 or 9 digits, by the value. The daemon parses the time when it is there. A line without it, which an older initrd wrote, gives a record with the code or the signal and no time. A time that is not RFC 3339, or is empty, is read as no time and the code or signal is still read: the line says the entrypoint exited, and a bad time must not turn that into a cut-short record.
3. **The daemon stores the time with the exec record, passes it through unchanged, and never puts a time of the node's in its place.** A time that is not known is unset.
4. **The time is the guest's clock.** The host sets that clock at `Boot` ([ADR 0044](./0044-boot-sets-the-guests-wall-clock-to-the-hosts-as-catchup-does.md)) and when it restores a Machine (`CatchUp`, [ADR 0016](./0016-a-restored-guest-reseeds-from-host-entropy-and-takes-its-time-from-the-host.md)), and between those it is the guest's own. The runtime API's field comment says so.

## Consequences

- A client learns when a command ended however late it reads, including after a daemon restart, since the record is stored with its time, and after the VMM ended, since the line carries it.
- The time is as good as the guest's clock. A guest that boots with its clock off by some amount, or whose clock steps, has a time off by that much, and comparing it with a time the node reads is the client's to do knowing so.
- The agent stamps the time when the wait on the child returns. Finishing the record waits up to one second more for the output pipes to drain, so the time is of the end of the process and not of the record's completion.
- An exec record stored before this change, and an entrypoint line from an older initrd, have no time and read back with none.

Candidates weighed:

- **Who stamps the time.**
  - **The Guest Agent, at the reap** — chosen. It is the only party that sees the moment, and the stamp survives a read made later, from another connection, or from the log.
  - **The daemon, when it learns of the end.** It would be the time of a look, differing by how late the look was, and for a Machine whose VMM ended on its own, of the first read after. A host time passed as the command's would also look as exact as the guest's.
  - **The daemon, as a fallback where the guest gave none.** Rejected for the same reason, and because a record would then mix two clocks with no sign of which.
- **Where the entrypoint's time reaches the daemon.**
  - **A trailing ` at=` on the existing lines** — chosen. The line stays one line the agent writes at one moment, and the new parser reads the old form. A daemon that predates the change fails to read the number before the time, so the daemon and the initrd it pairs with are changed together.
  - **A new line for the time.** It could be lost apart from the line it belongs to, and the log has no framing to pair them.
- **A malformed time.**
  - **Read the code or signal, drop the time** — chosen.
  - **Treat the line as unreadable.** It turns a known exit into a cut-short record for a field that is only extra.

## Signs the decision was wrong

- A client that needs the node's clock for the end and compares the two, which would mean the record should carry both.
- A guest clock that is wrong often enough at boot that the time misleads more than it tells.
- A console line with a time that a reader cuts on wrongly, such as a reason text that contains ` at=`.
