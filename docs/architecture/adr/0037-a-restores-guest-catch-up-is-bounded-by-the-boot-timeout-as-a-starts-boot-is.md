# ADR 0037: A restore's guest catch-up is bounded by the boot timeout, as a start's guest boot is

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#278. Start and Restore bring a VMM up by the same sequence, and the guest step that ends it was bounded differently. Start's `Boot` runs under the lifecycle's own `boot_timeout`, which holds when the Guest adapter ignores its context (`runtime-api.md`, the note "Start waits for the guest's `Boot`"). Restore's `CatchUp` ran on the call's context, so its bound was the call's `call_timeout`, and only if the adapter honoured the context. A guest that never answers `CatchUp` kept the Machine's lock for the whole call bound, and an adapter that ignored its context kept it past that.

[ADR 0016](./0016-a-restored-guest-reseeds-from-host-entropy-and-takes-its-time-from-the-host.md) makes the Machine not Running until the guest has caught up, so a catch-up that does not return is a restore that does not end.

Candidates weighed for the bound of the catch-up:

- **The lifecycle's own `boot_timeout`, as for Start's `Boot`** — chosen. One bound for the one step both calls end with, held by the lifecycle whatever the adapter does with its context.
- **The call's bound only, as before.** It holds only if the Guest adapter honours its context, so one adapter that does not leaves a restore held until the call is ended some other way, and Start and Restore differ for no reason the contract states.

## Decision

1. **The guest step that ends a bring-up is bounded by `boot_timeout`, for a start's `Boot` and for a restore's `CatchUp`.** The bound is the lifecycle's own timer: it holds when the caller's context never ends and when the Guest adapter ignores its context, and a caller that gives up does not cancel the step.
2. **A catch-up that has not returned within `boot_timeout` fails the restore** as a failure of the kind its row names: no Machine exists, or Faulted if its VMM cannot be ended. The reason is "the guest did not finish catching up within <timeout>".
3. **One module runs the bring-up for both calls.** The order of the steps, the wait for the VMM, the bounded guest step and the unwind of what the sequence began (the VMM it started, then the network it applied) are written once. Each call chooses the Driver's description, the Driver's VMM step and the guest step, and maps the outcome to its own state. The two outcomes stay as `runtime-api.md` states them: a start that began and failed is Stopped with the reason, and a restore that failed leaves no Machine.

## Consequences

- A restore takes at most `boot_timeout` longer than its VM's wait, as a start does, and no adapter can stretch it.
- A catch-up the lifecycle gives up on may still be running in the adapter when the VMM is ended; ending the VMM ends it, as for a `Boot`.
- A restore whose network could not be removed when the VMM was ended says so in its reason.
- This rules out a bound on the guest step that depends on the adapter honouring its context.

## Signs the decision was wrong

- A guest whose catch-up takes longer than `boot_timeout` on a healthy node, which would call for a bound of its own for the catch-up rather than the boot's.
- A Guest adapter whose catch-up cannot be abandoned half-done, so that giving up on it leaves the guest in a state the VMM's end does not clear.
