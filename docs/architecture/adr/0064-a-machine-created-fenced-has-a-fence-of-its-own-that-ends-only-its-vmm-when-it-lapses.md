# ADR 0064: a Machine created fenced has a fence of its own that ends only its VMM when it lapses

> Status: accepted
> Date: 2026-11-14

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The fencing deadline is one deadline for the node: when it passes, mvmd stops every Machine under it ([runtime-api.md](../runtime-api.md) §Fencing deadline). A client that orchestrates a stateful member across nodes needs to fence that member alone, before it starts a replacement elsewhere, and to confirm that the fence took (garamsh/aurora#811). The node's deadline cannot serve: it would end every other Machine on the node.

The supervisor of each Machine already holds an expiry of its own, and the fencing service sets it through `SetExpiry`, which names the Machine ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)). The Machine's end is already observed through the supervisors the daemon lists ([ADR 0033](./0033-a-vmm-that-ends-on-its-own-moves-its-machine-to-stopped-with-the-reason.md)), and a Machine marked for a restart waits for an armed deadline ([ADR 0062](./0062-a-machine-created-with-restart-on-boot-that-was-running-when-its-host-went-down-is-started-again-at-the-first-arming-in-the-new-boot.md)).

## Decision

A Machine can be created with a fence of its own, a deadline the client renews that is independent of the node's. When it lapses, the Machine's supervisor ends that Machine's VMM and no other.

- **The field.** `fenced`, a bool, default false, on `CreateMachineRequest` and reported on `Machine`. Both forms take it. It is additive in `v1alpha2`, fixed for the Machine's life, and not taken by Restore Machine. The fence is not armed at creation.
- **The calls.** Renew deadline and Inspect deadline each take an optional Machine identifier. Empty is the node's deadline, as before. A name is that Machine's fence: an unknown Machine is not found, and a Machine created without the field is precondition failed. The rules are the node's: a positive length, a renewal number that must be greater than the last applied to change anything, the length counted from receipt, and an acknowledgement only after the record is durable and the Machine's supervisor holds the expiry. A renewal moves no other Machine's expiry. Its phase is armed, not armed or expired, and never "stopped everything".
- **The effective expiry.** The supervisor of a fenced Machine holds the earlier of the node's expiry and the Machine's own, set through the same `SetExpiry` call at each start, each renewal of either and each reconciliation. The ordering rules of ADR 0003 hold for each Machine: a shortening reaches the supervisor before it is recorded, and a supervisor that refuses it is killed. Neither the supervisor nor its protocol changes. When the node's deadline passes first, mvmd stops every Machine as before.
- **Start.** A start of a fenced Machine is refused while its fence is not armed, whatever the node's deadline. A fence is not armed after creation, after a lapse and after a host reboot.
- **What a renewal after a lapse does.** It arms the fence again and starts nothing, as a renewal of the node's deadline does. The Machine stays Stopped until the client starts it.
- **The record.** A fence is held in its own record, in the deadline's bucket, by the fencing service and not in the Machine's record, because a Machine's lock is held for the whole of a start and a renewal must not wait for it. It is made at Create and removed at Delete.
- **A lapse told apart.** A Machine's VMM found ended counts as ended by its fence when the Machine's fence, armed in the current boot, has an expiry that has passed, and the node's deadline did not pass earlier. mvmd compares on the boot clock the supervisor counts. The Machine is then Stopped with a reason that begins "the Machine's own fence lapsed and ended its VMM" and gives the cause the console shows, and Inspect reports the time the fence lapsed, `fence_lapsed_at`, which a later start clears. The same test runs when mvmd starts, so the finding holds across an mvmd restart and for a VMM that ended while mvmd was down.
- **ADR 0062.** A Machine whose fence lapsed in the boot is not restarted, as any end in its boot. A fenced Machine marked for a restart after a host reboot is not started until its fence is armed in the new boot, because the fence is not armed after a reboot and Start refuses; the mark stands and the daemon starts it at the next look after the client renews the fence. The look asks the Start Permitter for each marked Machine and no longer for the first only, so a Machine with an unarmed fence does not hold the others back.
- **What is not decided here.** No fence applies to a group of Machines or to anything but a Machine, and no event kind is new: the Machine's Stopped event is the notice.

## Consequences

- A client fences one Machine by not renewing it, and confirms it by Inspect Machine: Stopped, a reason naming the fence and the time.
- Every other Machine keeps running, including one with a fence of its own that is renewed.
- The lapse is told by comparison: a VMM that crashed shortly before the fence's expiry and was found after it reads as a lapse. The reason's cause line is still the console's.
- For a Machine's own fence only the supervisor counts. The node's deadline has mvmd kill each supervisor at the expiry as a second count; a Machine's fence has none, so its enforcement is the supervisor's alone, and the daemon learns of the end by the look it already makes.
- The effective expiry is a function of two values, and a renewal of either can change what the supervisor holds.
- A Machine with the field and a host reboot needs the client's renewal of its fence as well as the node's before it restarts.

## Signs the decision was wrong

- A client needs the fence to hold a group of Machines, or one that outlives the Machine.
- A lapse is confirmed for a Machine whose VMM ended for another reason, often enough that the client acts on it wrongly.
- The supervisor's count alone proves too weak and a second count by mvmd is needed.
