# ADR 0044: Boot sets the guest's wall clock to the host's, as CatchUp does

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#363. The e2e test of the exec record's `ended_at` (garamsh/mvmd#331, PR #360) found the guest's wall clock 266 to 381 ms behind the host's after a cold boot. A guest starts with the clock its VMM and kernel give it, and nothing on the boot path corrects it. [ADR 0016](./0016-a-restored-guest-reseeds-from-host-entropy-and-takes-its-time-from-the-host.md) sets the clock only through `CatchUp`, which a restore sends and a boot does not, so a booted guest's times (an exec's `started_at` and `ended_at`, file times, a workload's own timestamps) are on a clock that disagrees with the host's.

Candidates weighed:

- **Leave the clock to the VMM and the kernel.** Both VMMs hand the guest some time at boot, but neither document read for ADR 0016 promises it is the host's wall clock to within a call's latency, and the error measured above is not.
- **Have the daemon correct times it reports from the guest.** A host time would then stand in for a guest's time, which ADR 0043 (reserved by PR #360) decision 3 rules out.
- **A separate call that sets the clock after `Boot`.** It would be a second step a Start can lose between, and the clock would be wrong while the entrypoint starts.
- **A field of `Boot`, set before any other step** — chosen. It is the shape `CatchUp` already has, on the call that already carries the boot description, and the entrypoint never runs on the wrong clock.

## Decision

1. `BootRequest` gains `int64 host_unix_nanos = 5`, the host's wall clock in nanoseconds since the Unix epoch at the time the daemon makes the request. The addition keeps `v1alpha1`.
2. A positive value makes the agent set `CLOCK_REALTIME` with `clock_settime`, through its `Clock` seam, before it mounts anything or starts the entrypoint. A failure of that call is a boot step, `BOOT_STEP_CLOCK` (first in the order, shown to people as "set the wall clock"), so the status detail `BootFailed` names it as it names every other step. The value 0 leaves the clock as the kernel set it. A negative value is refused as invalid, and nothing is set or started.
3. A repeat of `Boot` is judged by the description without the time. A repeat of a boot that succeeded succeeds and does not set the clock again, whatever time it carries, so a repeat after a lost answer cannot move the clock back. A repeat after a failure fails as before.
4. The daemon fills the field from the same wall clock it gives `CatchUp`, the lifecycle's clock. The Guest port reads that clock after the agent has answered `Info` and immediately before it sends `Boot`, and not when the lifecycle builds the description: the port waits for the agent to listen, with backoff, and a time read before that wait would be older than the guest's clock by the wait.
5. This amends [ADR 0016](./0016-a-restored-guest-reseeds-from-host-entropy-and-takes-its-time-from-the-host.md)'s scope for the clock from "a restore" to "a boot and a restore". Nothing else in ADR 0016 changes: the restore still sets the clock through `CatchUp`, and the monotonic clock is still the VMM's.

## Consequences

- A booted guest's wall clock is right to within one call's latency and the agent's time to reach the step, on either VMM, as a restored guest's is.
- The agent holds one more guarded step. It needs `CAP_SYS_TIME`, which it has as PID 1.
- A caller of the Guest port that sends no clock (the field's zero) gets the old behaviour, which keeps a test double and an older daemon working against a newer agent.
- Drift over a long run is not corrected: the clock is set once per boot, and a guest left running keeps whatever rate its clocksource gives. Nothing here promises the clocks agree later.
- No host time substitutes for a guest's time anywhere; what changes is the guest's own clock.

## Signs the decision was wrong

- A guest's clock is found to disagree with the host's by more than a call's latency right after a boot, so the stamp is stale by something the Guest port does not account for.
- Setting the clock before the mounts breaks a guest whose kernel or clocksource cannot take a step at that point, so the step has to move after another.
- Workloads need the clock kept right over a long run, so a one-time set at boot was the wrong place and the agent needs a periodic or VMM-provided correction.
