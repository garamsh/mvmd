# ADR 0040: A Driver states the most of a Machine it accepts, and the inside refuses a request above it

> Status: accepted
> Date: 2026-11-11

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#306, reported by gaon's PM: gaon's scheduler (garamsh/gaon#5) places Machines from runtime info.

- Both Drivers refuse more than 32 vCPUs in `Describe`, which the lifecycle calls at Start (`daemon/internal/firecracker/describe.go`, `daemon/internal/cloudhypervisor/describe.go`). Firecracker's figure is the cap of v1.17.0's `MachineConfiguration` in its swagger; Cloud Hypervisor v54.0 accepts more, and its 32 is a bound the project chose. Cloud Hypervisor also holds the ceiling's vCPUs to it. Neither Driver checks an upper bound on memory.
- The lifecycle checked a Machine's vCPUs only for zero. A Create Machine of 64 vCPUs was accepted and failed at its first Start, and a Restore or a live resize could meet the same bound later.
- Runtime info reported a Driver's name, version, CPU model and capabilities, and no limit, so a client could not tell the largest Machine the node accepts.
- [ADR 0039](./0039-runtime-info-reports-the-nodes-capacity-and-mvmd-admits-no-call-by-it.md) reports what the node has and admits no call by it. A Driver's limit is a different figure: what the VMM refuses regardless of the node's capacity.

Candidates weighed:

- **The Driver states its limits, and the lifecycle and the API report them** — chosen.
- **The lifecycle holds a limit of its own.** Rejected: the figure belongs to the VMM, so a Driver added later would need the lifecycle changed, against [driver.md](../driver.md)'s rule that no VMM's vocabulary appears in the contract.
- **Check only in `Describe`, as before.** Rejected: the refusal comes at Start, after the Machine, its disk and its network exist, and a client cannot read the bound before it asks.
- **Move the check out of `Describe`.** Rejected: a Driver that is handed a description it cannot run should still refuse it, so the Driver keeps its own check and states the same figure once.

## Decision

1. **`DriverInfo` carries `Limits`, the most of a Machine the Driver accepts.** It holds the most vCPUs, the only upper bound either Driver checks. A zero figure states no bound, and a Driver that states none reports none.
2. **A Driver defines each limit once and uses it for both `Info` and `Describe`.** `Describe` keeps refusing a description above it.
3. **The inside checks the limits before anything is made.** Create Machine (the resources and the ceiling) and Set resources (the target and the ceiling, in any state) refuse a figure above a limit as invalid, which the runtime API reports as REJECTED. Restore (the snapshot's resources and ceiling) refuses it as a failed precondition, which the runtime API reports as PRECONDITION_FAILED. No record, disk, address or process exists after the refusal, and it is not remembered under the request ID.
4. **Runtime info reports them.** `DriverInfo` gains the optional `limits`, a `DriverLimits` message with `max_vcpus`, absent when the Driver states none. The version stays `v1alpha2` ([runtime-api.md](../runtime-api.md) §Scope and version).
5. **A Restore above a limit is a failed precondition, not an invalid request.** The request, a snapshot id and a policy, is well formed; the snapshot's figures, not the request's, exceed the limit, as a snapshot made by another Driver does not fit this node's, which Restore already refuses as a failed precondition. Create Machine and Set resources carry the figures in the request, so there they are invalid. A snapshot made by a Driver of the same name, version and CPU model can still hold figures above the limit the Driver states now.

## Consequences

- A client reads the largest Machine from runtime info and sends no request the node refuses for it.
- A Machine record or snapshot made under a higher limit is refused at Restore and at Set resources while its held ceiling is above the limit, and stops at Start as before.
- A Driver gaining a limit adds a field to `DriverLimits` and to its `Info`; the lifecycle checks it where it checks the others.
- The client-side validation in `lib/runtimeapi/v1alpha2` does not know the limit, because it varies by Driver.

## Signs the decision was wrong

- A Driver's bound depends on more than one figure (vCPUs against memory, or on the host), so a single maximum per field misstates what it accepts.
- A limit that the Driver enforces in `Describe` and drifts from the figure `Info` reports, which the Drivers' tests are there to catch.
- A client that wants the limit before it chooses a Driver, because the node serves more than one.
