# ADR 0062: a Machine created with restart_on_boot that was Running when its host went down is started again at the first arming in the new boot

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

[ADR 0033](./0033-a-vmm-that-ends-on-its-own-moves-its-machine-to-stopped-with-the-reason.md) left restart policy to whoever drives mvmd. The decision to change that was taken under garamsh/aurora#804 and recorded for garamsh/mvmd#664: after a host reboot every VMM is gone, the fencing deadline is not armed ([runtime-api.md](../runtime-api.md) §Fencing deadline), and a client that has not yet renewed cannot start anything, because Start needs an armed deadline. A Machine that was Running stays Stopped until the client has re-armed the deadline and asked for each start again.

What mvmd could not tell before: whether a Machine's VMM ended because the host's boot changed or in the same boot, and whether a Machine was being started when the host went down.

## Decision

A Machine created with `restart_on_boot` is started again by mvmd, when its deadline is first armed in the new boot, if it was Running when its host went down. This amends ADR 0033 in its last bullet, "Restart policy is not mvmd's", and no more: the report of a VMM's end, its reason and its state stand.

- **The field.** `restart_on_boot`, a bool, default false, on `CreateMachineRequest` and reported on `Machine`. Both the image form and the OS form take it. It is additive in `v1alpha2`, so the version stays, and it is fixed for the Machine's life. Restore Machine does not take it.
- **What is recorded.** A start of a Machine with the field records the host's boot identity (the identity the fencing deadline already compares, [ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)) in the Machine's record before the boot begins, so that a start in progress is recorded as well as a run. Any state other than Running and Paused clears it. A Machine without the field records none.
- **The conditions.** After mvmd starts, Restore settles Machines as before. A Machine is then started if all three hold:
  - it has `restart_on_boot`;
  - it was Running, or being started, when its VMM ended, and the identity recorded is not the host's current boot;
  - no Stop, Pause or Delete was asked after it last ran. A Stop or Delete after Restore, before the start, cancels it; a Machine that was Paused when its VMM ended is not started.
- **When.** The start is made when the Start Permitter first permits starts, which is when the fencing deadline is first armed in the boot. The daemon looks for such Machines at the interval it looks for VMM ends, so the start follows the arming by at most that interval. A permit that refuses leaves the Machine marked for the next look.
- **Same-boot ends.** A VMM that ended in the boot it started in (a crash, an out-of-memory kill, a fencing expiry) is not started again: the identity recorded is the current one.
- **A failed restart.** The Machine stays Stopped, and its reason begins "the restart after the host's boot changed failed" and names the cause. It is not retried. The usual state events are emitted, and the record keeps the reason.
- **Fencing is unchanged.** The deadline, its arming and the expiry are as before; the restart is an ordinary Start under the armed deadline.
- **No setting.** The boot identity is the one the daemon already reads from the host. There is no client-visible or operator-facing way to change it; tests give the daemon a clock that reports another boot.

## Consequences

- A client that wants a Machine to survive a host reboot sets the field once, and needs no start of its own after it has armed the deadline.
- A Machine's record holds one more value, and a Machine with the field records it at each start.
- A restart is a Start the client did not ask for: it carries the Machine's recorded env and files, and an image or boot set that is no longer on the node fails it, with the reason above.
- A Machine Stopped by the client before the host went down stays Stopped, and so does one whose reason is a crash.
- ADR 0033's sentence that whoever drives mvmd decides whether a Machine that ended is started again holds only for a Machine without the field, and for a VMM that ended in its boot.

## Signs the decision was wrong

- Clients need a restart after a VMM end in the same boot, which would be a restart policy and not a boot rule.
- A restart that fails needs another attempt, or a delay, to be of use.
- The interval between the arming and the start is too long for a client that waits on the Machine.
- A client needs to change the field on an existing Machine.
