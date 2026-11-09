# ADR 0018: A graceful stop goes through the Guest Agent, and the VMM request is the fallback

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#86, from the finding of PR #84 (garamsh/mvmd#30). A graceful stop of a Machine asked the VMM to deliver a keyboard or power request to the guest kernel: Firecracker's `SendCtrlAltDel` needs the guest's i8042 keyboard controller (`CONFIG_SERIO_I8042`, `CONFIG_KEYBOARD_ATKBD`), and the pinned Kata Containers 4.3.1 kernel has `# CONFIG_SERIO is not set` ([guest.md](../guest.md) §Kernel candidates checked). Cloud Hypervisor's ACPI power button depends on the kernel in the same way. So on the pinned kernel the request never arrives, every graceful stop waits out its whole grace, and then the supervisor ends the VMM.

Forces:

- Every Machine runs the Guest Agent as PID 1 ([guest.md](../guest.md)), and its Termination rule already says what a graceful stop means: forward `SIGTERM` to the entrypoint, or end the Machine when the entrypoint is not running. Only the kernel's signal to init reached that rule before this decision.
- The agent is reached over vsock, which every Machine has whatever its VMM and kernel ([ADR 0014](./0014-the-guest-agent-serves-a-grpc-contract-it-owns-over-vsock.md)).
- Two ways to reach one rule must not diverge, so they cannot be two implementations of it.
- The agent can be unreachable (not yet booted far enough to listen, or hung), so the VMM-level request keeps a use.

Candidates weighed:

- **A `Terminate` call on the Guest Agent, applied by the function the init's signal handling uses, with the VMM request as the fallback when the agent cannot be reached** — chosen.
- **Choosing a kernel that has i8042.** It ties stop to one VMM's device model, and Cloud Hypervisor's path would need its own kernel condition.
- **`Signal(entrypoint, SIGTERM)` alone.** It says nothing when the entrypoint is not running, a case the Termination rule already settles by ending the Machine, and before `Boot` it is refused as a failed precondition.
- **A second implementation of the rule in the service.** Two copies of what a termination does would drift; the call is carried to the one the init uses.

## Decision

- **The Guest Agent serves `GuestService.Terminate`**, an addition to `v1alpha1` (`guest-agent/proto/mvmd/guest/v1alpha1/guest.proto`). It applies the Termination rule exactly as a signal to init does: it forwards `SIGTERM` to the entrypoint, or ends the Machine when the entrypoint is not running. The agent sends the call into the same supervision loop as the init's signals, and one function applies the rule to both.
- **The request carries no signal.** A graceful stop is `SIGTERM`, which is what a client of the call means; `SIGINT` reaches the agent from the kernel on a ctrl-alt-del and has no client to name it. A field for a signal would let a client ask for the other signals through a call whose rule is about stopping, where `Signal` already delivers any signal to a running command. An addition to `v1alpha1` can add a field later without breaking a client.
- **It is served before `Boot` has succeeded**, like `Health` and `Info`. Before the entrypoint runs, the rule is to end the Machine, which is what a stop of a Machine that never booted needs.
- **It returns when the agent has taken the request**, not when the Machine has ended: the entrypoint may take the whole grace, and the end of the Machine is observed on the VMM. A repeat sends `SIGTERM` again, so the call is safe to repeat; an agent that is already ending the Machine refuses it as a failed precondition.
- **The host-side `guest-agent-channel` binary refuses it** as a failed precondition, since it is not the guest's init and has no Machine to end.
- **The daemon's graceful stop asks the agent first**, through a `Terminate` operation on the Guest port, then waits up to the grace for the VMM to end. The Driver's VMM-level `Shutdown` is used only when the agent cannot be reached (`ErrGuestUnreachable`). The forced end after the grace is unchanged: a stop always ends with the VMM terminated ([runtime-api.md](../runtime-api.md)).

## Consequences

- A graceful stop reaches every guest the same way whatever the VMM and the kernel, and a Machine whose entrypoint handles `SIGTERM` ends before the grace instead of after it.
- The VMM-level request stops being the main path and the Driver's `Shutdown` is exercised only on a Machine whose agent is down.
- The Guest Agent's contract grows by one call, the daemon's generated Go client changes with it, and the Guest port gains an operation every implementation of it serves.
- A stop of a Machine whose agent is hung but reachable at the transport still waits out the grace, as before.

## Signs the decision was wrong

- A guest whose agent is up and whose `Terminate` is accepted does not end before the grace in the daemon's tests, so the call reaches something other than the Termination rule.
- A client needs a signal other than `SIGTERM` for a graceful stop, so the empty request was too narrow.
- The Driver's `Shutdown` is never used on a real node, which would show the fallback has no case left and can go.
