# ADR 0032: The Guest Agent ends the Machine by the method the kernel command line names

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#221, found by the `e2e` job of garamsh/mvmd#220 (CI run 42789515763). A Machine whose entrypoint exits with code 7 ended the VMM on one Driver and not on the other:

- **Firecracker v1.17.0, x86_64.** The VMM's log ends with `mvmd-guest-agent: entrypoint exited code=7`, then `reboot: Power off not available: System halted instead`. The kernel halts and the VMM keeps running; the test timed out after 2 minutes waiting for the VMM to end.
- **Cloud Hypervisor v54.0, x86_64.** `reboot: Power down`, and the VMM ends.

What the agent did in `guest-agent/` at commit 54b5f4b39f1c0847922702a9a540c1a0dd7f9fad: every end of the Machine (the entrypoint's end, a termination before it ran, the agent's own failure, a panic) called `reboot(RB_POWER_OFF)` and fell back to `reboot(RB_AUTOBOOT)` only when that call returned. A halt does not return, so the fallback was never reached. This settles the question `guest.md` listed as open, whether `reboot(RB_POWER_OFF)` ends a Machine on each VMM: not on Firecracker x86_64.

How a guest ends its VMM is a property of the VMM and the architecture. The Driver knows its VMM and the agent knows none.

Candidates weighed:

- **The Driver names the method on the kernel command line it builds per architecture; the agent obeys** — chosen. The agent must end the Machine even when it fails before `Boot` (`guest.md` §PID 1 duties, "It never returns"), and the command line is the one input it has by then.
- **The method travels in the `Boot` call.** Rejected: an end before `Boot` has succeeded would not know it.
- **The daemon kills the VMM when it reads the agent's last console line.** Rejected: a VMM nobody ends whenever the daemon is down.
- **Always reset.** Rejected: on a VMM that treats a reset as a reboot, the Machine would boot again.

## Decision

The kernel command line carries one parameter, `mvmd.end`, set by the Driver. It names one of two methods:

| Value | The agent calls |
|---|---|
| `poweroff` | `reboot(RB_POWER_OFF)` |
| `reset` | `reboot(RB_AUTOBOOT)`, which `reboot=k` turns into the keyboard-controller reset on x86 |

- **The agent reads `/proc/cmdline` once**, as the step after it has mounted `/proc`, and parses the line into one typed value at that boundary. When the line holds `mvmd.end` more than once, the last counts. Every end uses that value.
- **Absent or unknown.** A line with no `mvmd.end`, any value other than the two, or a command line that cannot be read, is written to the console as one `mvmd-guest-agent: ` line, and the agent ends by power off. That keeps what the agent did before this decision for a Driver that does not set the parameter, and a Machine on Cloud Hypervisor still ends. No reason against it was found: the other default, reset, would boot a Machine again on a VMM that treats a reset as a reboot.
- **The other method is the fallback.** When the named method returns, the agent tries the other, and when that returns too it waits for ever.
- **An end before the parameter is read is a power-off.** That is a failure to mount `/proc`, or a panic or a failed runtime build before the agent reads it.
- The Drivers set the parameter in garamsh/mvmd#222. Until they do, every Machine takes the absent case.

## Consequences

- The agent states no VMM's behaviour, and a new VMM or architecture is a Driver's change alone.
- Every Machine's console log carries one more line until the Drivers set the parameter.
- A Machine that fails to mount `/proc` ends by power off, which does not end a Firecracker x86_64 VMM.
- A fallback to the other method can reboot a Machine on a VMM that treats a reset as a reboot, but only after the named method has returned, which a working method does not.
- The Machine must not reboot on any shipped Driver. That is verified only when the Drivers' half lands, by the end-to-end test of garamsh/mvmd#220.

## Signs the decision was wrong

- A VMM or architecture needs a method that is neither power off nor reset, and the parameter grows a third value.
- A Machine's end has to be chosen after boot, by something that is not the Driver, and the command line is too early to hold it.
- Machines on a Driver that sets the parameter still reach the absent case, so the command line does not arrive intact.
