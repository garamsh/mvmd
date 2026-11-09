# Runtime API

The operations mvmd offers its client, the rules every call follows, and the version this contract carries.

Checked against garamsh/discussions#2 as read on 2026-11-08 (decision-change reply discussioncomment-19466961, §3 discussioncomment-19288319, §4 discussioncomment-19288320, §5 discussioncomment-19288321) and against garamsh/mvmd#4. Where neither settles a point, it is under Open questions and binds nothing.

## Scope and version

- **The API models Machines.** It does not model Pods or containers and is not CRI (#2 §1 conclusion; [runtime.md](./runtime.md) §Runtime API scope). A Machine is one microVM on this node.
- **It names nothing of its client.** No type, resource or identifier of the client appears in it. The client is whatever calls it; mvmd does not know what that is.
- **The version is `v1alpha1`.**
- **Encoding and transport.** A gRPC service defined in Protocol Buffers, served on a Unix domain socket on the node; the socket's filesystem permissions are the authentication. The protobuf package and the Go package path carry the version, and the generated code is committed ([ADR 0004](./adr/0004-the-runtime-api-is-grpc-on-a-unix-socket-with-committed-generated-code.md)).
- **Rule for changing the version.** A change that adds an operation, an optional field or a capability keeps the version. A change that removes or renames anything, changes what an operation means, or changes its idempotency rule or its state after failure is a new version identifier (`v1alpha2`, then `v1beta1`, and so on). The reason is that the client builds against the version number: it must be able to tell that nothing it relies on moved. The rule has no exception for the alpha stage, so the client can build against `v1alpha1` as soon as this document is merged.
- **Versions in service.** mvmd reports the versions it serves in the runtime info call below, and a call naming a version it does not serve is refused as unsupported. How many versions are served at once is an open question.

## Calling rules

- **Identity.** The client chooses the identifier of every Machine, snapshot, disk and exec record it creates, unique on this node. The reason: if the connection drops between sending a creating call and reading its answer, the client already knows what to look up, and no object exists that the client cannot name.
- **Request ID.** Creating calls (create Machine, restore, snapshot, exec) also carry a client-chosen request ID (#2 §5 design principle 3; #2 §3 principle 5). The same identifier with the same request ID and the same parameters returns the original outcome. The same identifier with a different request ID or different parameters is a conflict and changes nothing. mvmd remembers a request ID for as long as the object it created exists.
- **State-targeting calls need no request ID.** Start, stop, pause, resume, delete, attach, detach and resize name a target state. Repeating one when the Machine is already in that state succeeds and changes nothing, so a retry is always safe.
- **Outcome kinds.** A call ends in one of: success; rejected as invalid; not found; conflict (identifier in use with other parameters); precondition failed (the Machine is not in a state that allows the call, or a rule such as fail-closed start or one writer per disk forbids it); unsupported (the Driver lacks the capability, [driver.md](./driver.md)); or failed (the attempt ran and did not finish). Rejected, not found, conflict, precondition failed and unsupported change nothing. Each failed outcome leaves the state the tables below name.
- **An unknown outcome is not an API result.** If the client loses the connection or times out, it does not know the outcome. An accepted call is not cancelled by a disconnect: it completes or fails into its defined state. The client resolves the doubt by repeating the call (same identifier, same request ID), which returns the final outcome or reports that the call is still in progress, or by reading the Machine back.
- **An accepted call runs to its end, within a bound.** A state-changing call (create, start, stop, pause, resume, delete, attach, detach, resize, snapshot, restore) is accepted when it holds its Machine; from then on the client's disconnect or deadline does not reach its steps. A call still waiting for its turn on the Machine is not yet accepted in this sense: it runs when its turn comes, whether or not its client is still there, and then it is bound like any other. A bound that mvmd applies ends a step that hangs, so a call cannot hold its Machine for ever: the node's `call_timeout` (default 10 minutes, at most 6 hours) from the moment the call holds the Machine, plus a start's `boot_timeout` or a stop's grace period. The bound relies on each port honouring the context it is given. The steps that undo what a failed call did run on a context of their own with a short bound of their own, so a call the bound ends still reaches the failure state its table row names.
- **A repeat while the first call runs waits.** Calls on one Machine run one at a time, so a repeat with the same identifier and request ID returns once the first call has an outcome, and returns that outcome; it never starts the work a second time. A client that cannot wait reads the Machine back instead.
- **Reads are never state.** Inspect, list and events change nothing and can be repeated freely.

## Machine states

| State | Meaning |
|---|---|
| Created | Configured on the node, never booted or last stopped before boot. No VMM process. |
| Running | Booted or restored; the guest executes. |
| Paused | The guest is stopped with memory held. |
| Stopped | Not executing; disks retained. |
| Faulted | mvmd cannot establish the Machine's state, for example a stop that could not be confirmed. The only calls that apply are stop, delete, inspect and reads. |

Inspect reports the state, the reason for the last failure if any, the Driver and its version, the Machine's disks and resources as mvmd holds them, and its network addresses, one per family its node serves, and the network policy in effect.

## Operations

"Repeat" is the result of the same call made again.

### Machine lifecycle

| Operation | Needs | On success | Repeat | State after failure |
|---|---|---|---|---|
| Create Machine | Image already on the node, resources, root disk size, disks, network policy | Created | Same request ID: original outcome | No Machine exists |
| Start | Created or Stopped; network policy applied; fencing deadline armed | Running, once the VMM runs and the guest has been sent its boot description and its entrypoint runs | Already Running: success. A repeat while a start is in progress waits and returns that start's outcome | Prior state; a boot that began and failed, whether the VMM's or the guest's, ends in Stopped with the failure reason, which names the guest's boot step when one failed. A guest that has not finished booting within the node's `boot_timeout` (default 2 minutes, at most 1 hour) is such a failure: the VMM is ended and the reason is "the guest did not finish booting within <timeout>" |
| Stop | Any state | Stopped | Already Stopped, or Created: success, no change | Running or Paused if the stop never began; Faulted if it began and cannot be confirmed, or if the VMM ended but the Machine's network could not be removed, and a later Stop retries |
| Pause | Running | Paused | Already Paused: success | Running |
| Resume | Paused | Running | Already Running: success | Paused |
| Snapshot | Running or Paused; Driver supports snapshot; no disk attached but the root disk | Machine in the state it was in; snapshot exists | Same request ID: the existing snapshot | The Machine in the state it was in, and no snapshot exists; Faulted if the Machine cannot be returned to it |
| Restore | A snapshot compatible with this node's Driver, version and CPU; fencing deadline armed; network policy applied | New Machine, Running | Same request ID: original outcome | No Machine exists; Faulted if its VMM cannot be ended |
| Delete Machine | Created, Stopped or Faulted | No Machine exists | Absent: success | Prior state |

- **Stop forces the guest down after a grace period the client names.** A guest cannot be trusted to stop (workloads are untrusted, #2 §5 security), so a stop always ends with the VMM terminated.
- **Start and restore are the two ways to Running** (#2 §4, node behaviour: "boot or restore"). Both refuse to run before the network policy is applied, so a Machine is fail-closed (#2 §4 invariant 6); the default network blocks all external traffic (#2 §4, core and extensions).
- **Start waits for the guest's `Boot`.** Running means the entrypoint runs ([guest.md](./guest.md) §Current decisions), so until `Boot` returns the Machine is reported in its prior state, for at most `boot_timeout`: the bound is the lifecycle's own, holds whatever the caller's context does, and a Stop sent meanwhile completes once it passes. A restore sends no `Boot`: the guest booted before the snapshot, and it is sent `CatchUp`.
- **Snapshot holds the guest paused for its duration** so memory and disk are one pair (#2 §2). It records the Driver name and version, the CPU model and the guest kernel ([driver.md](./driver.md)), and a snapshot never moves to another VMM.
- **Delete** exists because the client must be able to release a Machine's resources; the Driver contract has a matching destroy ([driver.md](./driver.md)).
- **Machine creation does not boot.** Create only configures, as the Driver's create does.

### Disks and resources

| Operation | Needs | On success | Repeat | State after failure |
|---|---|---|---|---|
| Attach disk | A disk the node holds; no other Machine on this node holding it, readers included; hotplug capability if Running or Paused | Disk attached | Attached with the same parameters: success | Not attached |
| Detach disk | Attached; hotplug capability if Running or Paused | Not attached | Absent: success | Still attached |
| Set resources | Absolute target (CPU, memory); resize capability if Running or Paused | Target held | Same target: success | The prior value; inspect shows the value in effect |

- **A disk has at most one holder on this node, readers included** ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)) (#2 §4 invariant 2): an attach that would add a second holder is precondition failed.
- **Resize names a target, not a delta**, so a repeat is idempotent.

### Network policy

Create Machine and Restore each carry a `NetworkPolicy`, the Machine's for its life ([ADR 0013](./adr/0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md)). mvmd enforces it on the node ([ADR 0012](./adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)) and assigns the Machine's address itself ([ADR 0011](./adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)).

| Message | Field | Meaning |
|---|---|---|
| `NetworkPolicy` | `egress` | Rules for packets the Machine sends. A packet leaves only if one matches. |
| `NetworkPolicy` | `ingress` | Rules for packets sent to the Machine. A packet arrives only if one matches. |
| `NetworkRule` | `prefix` | An IPv4 prefix (`a.b.c.d/n`) or an IPv6 prefix (`x:x::/n`), which sets the rule's family: the destination for egress, the source for ingress. One address is a `/32` or a `/128`. |
| `NetworkRule` | `protocol` | TCP, UDP, ICMP (IPv4 only), ICMPv6 (IPv6 only), or any protocol of the prefix's family. |
| `NetworkRule` | `ports` | Port ranges: destination ports for egress, the Machine's own ports for ingress. Empty means every port; only TCP and UDP rules may set it. |
| `PortRange` | `first`, `last` | Inclusive bounds, `1` to `65535`, `first` not above `last`. |

- **Empty is closed.** A policy with both lists empty, the default, blocks all traffic to and from the Machine (#2 §4, core and extensions).
- **Allow only, order-free.** A packet passes if any rule of its direction matches; there are no deny rules, and the order of rules means nothing.
- **Replies are implied.** Packets of a connection an accepted packet opened pass in both directions.
- **Two Machines on one node** reach each other only when the sender's egress and the receiver's ingress both allow it.
- **Invalid is rejected.** Any of these is rejected as invalid, and nothing changes: an unparseable prefix; ICMP with an IPv6 prefix or ICMPv6 with an IPv4 prefix; a port outside `1` to `65535`; a range with `first` above `last`; ports on an ICMP, ICMPv6 or any rule.
- **Both families.** A Machine gets one address per family its node serves ([ADR 0011](./adr/0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md)). A rule for a family the node does not serve is valid and matches nothing there.
- **Neighbour discovery is not policy.** mvmd admits the ICMPv6 a Machine needs to resolve its gateway and be resolved, and drops router advertisements, redirects and router solicitations from it, whatever the policy says ([ADR 0012](./adr/0012-nftables-on-the-host-enforces-each-machines-policy-at-its-tap.md)).
- **DNS** is an address like any other: the guest's resolvers are the node's, and a policy reaching them allows their addresses on UDP and TCP port 53.
- **A snapshot carries no policy.** Restore's policy is the new Machine's, and the restored Machine has a new address.
- **Adding to it.** Name-based rules, a client-chosen address and an operation that replaces a policy are optional additions whose absence means what this table means, so they keep the version. A change to what an empty list means is a new version.

### Observation

| Operation | Result |
|---|---|
| Runtime info | The API versions served, the Drivers in service and their capabilities, so the client can match a Machine's required capabilities to this node (#2 §5 design principle 1) |
| Inspect | One Machine, as under Machine states |
| List | Every Machine on the node |
| Events | A stream of state changes, Machine deletions, deadline events and Driver events, each with the Machine identifier and a sequence number that grows on this node |

- **Events are resumable and not the source of truth.** The client may resume from a sequence number. If mvmd no longer holds events that far back, it says so and the client lists to resynchronise, so a missed event never leaves the client wrong (#2 §3, level-triggered reconciliation). How long events are kept is an open question.
- **One ordered sequence.** Events held after the requested sequence, then each new event, come as one sequence with no gap or repeat between them. A sequence of zero starts at the oldest event held, and a sequence equal to the latest one waits for the next event. Mvmd answers "resync required" and ends the stream when the events after the requested sequence are no longer held, when the sequence is ahead of what this node has emitted (its log starts empty at each start), or when a client that reads too slowly falls behind what is held; a stream never skips an event.
- **What is emitted.** Every committed change of a Machine's state, each change of the fencing deadline (armed, renewed, expired) and each event a Driver raises. A deleted Machine has no state to name, so Delete Machine emits its own event, Machine deleted, once the deletion has committed; a repeat that finds the Machine absent emits nothing. A stream ends cleanly when mvmd stops, and the client resumes from the last sequence it holds.

### Guest access

| Operation | Needs | On success | Repeat | After failure |
|---|---|---|---|---|
| Exec | Running; Guest Agent reachable; the command, and optionally an environment and a working directory | Command started under the exec identifier | Same request ID: the same exec record, never a second run | Machine unaffected; the exec record says it did not start |
| Read exec output | An exec record; an offset to resume after; Running, unless the record is already final, which is answered from the record | The output chunks after the offset, each with its offset, then the final exec record once the command has ended | Always safe; resumes from any offset still held | Machine unaffected; an offset no longer held is answered with the earliest offset held |
| Read file | Running | Content, in chunks | Always safe | Machine unaffected |
| Write file | Running | Whole new content or whole old content, never part (#2 §2, atomic write) | Same content: success | The old content |
| Patch file | Running; the base content digest and the digest expected afterwards | Content with the patch applied | File already at the expected digest: success | The old content |
| List and search | Running | Matches | Always safe | Machine unaffected |
| Console | Running or Paused | A session to the console | Another session; no state changes | Machine unaffected |

- **Exec is at most once per request ID.** mvmd never retries a command by itself, because a command has effects mvmd cannot see. If the Machine stops while a command runs, the exec record reports that it was cut short and why.
- **An exec record that mvmd cannot read back is that exec's alone.** After a restart, an exec and a read of its output under such an identifier fail, whatever the request ID and with the reason, and run or answer nothing; the Machine and its other exec records are unaffected, and Delete removes it with the Machine.
- **Exec output is read separately from starting the command**, so a client that lost its connection reads it again. Output is one sequence of bytes, stdout and stderr in the order mvmd received them, each chunk naming its stream and its offset in that sequence. A read resumes after an offset because the channel to the guest can reset (vsock connections reset on restore, [guest.md](./guest.md) §Rationale). The guest keeps only the newest output of each command, so an offset older than what it holds is answered with the earliest offset it holds, and the client resumes from there ([guest.md](./guest.md) §Current decisions). Once the command has ended, the exec record carries its exit code.
- **File content moves in chunks.** A read returns the file as a stream of chunks, so no file is bounded by one message. A write sends the path, then chunks, and mvmd commits the whole new content only when the stream ends cleanly; a stream that breaks leaves the old content, which is the whole-old-or-whole-new rule above.
- **Patch carries a precondition and a postcondition** so a retry after an unknown outcome is decidable without a request ID: the file is at the base digest (not yet applied), at the expected digest (applied), or elsewhere (precondition failed).

### Images

| Operation | Needs | On success | Repeat | After failure |
|---|---|---|---|---|
| Pull image | A reference that names a digest | Image converted and in the node cache | Already present: success | Nothing visible in the cache |

- **A reference names a digest, never a tag.** A tag can move, which would make a repeat mean something different from the original and would defeat the integrity check (#2 §5 image security).
- **An image becomes visible only after conversion and integrity verification finish** ([image.md](./image.md)), so a failed pull leaves no partial image. Each step of a pull and the state its failure leaves are listed in [ADR 0007](./adr/0007-the-node-image-cache-is-an-oci-image-layout-published-by-one-index-swap.md) §Pull image.
- **A boot checks the image.** Start and restore measure the image's root hash before the boot begins; a mismatch fails the call in its prior state, with the reason ([image.md](./image.md) §Integrity).

## Fencing deadline

The deadline exists so a Machine cannot run on two nodes when its client is gone ([runtime.md](./runtime.md) §Fencing). The client chooses its length; mvmd keeps it and acts on it. It is one deadline for the node, covering every Machine on it.

| Operation | Effect |
|---|---|
| Renew deadline | Carries a length and a renewal number. If the number is greater than the last applied, the deadline becomes the length from the moment mvmd receives the call, and the renewal number is recorded; otherwise nothing changes and the current expiry is returned. The first renewal arms the deadline. |
| Inspect deadline | The length, the time remaining, the last applied renewal number, and whether it is armed, expired, or stopped everything. |

- **The client sets and changes the length** with each renewal; a shorter or a longer one is accepted. The length is any positive duration: zero or a negative length is rejected as invalid, and nothing changes. mvmd imposes no other minimum or maximum, and the length it chooses is outside this document.
- **Renewal is idempotent and replay-safe.** A duplicate or a delayed older renewal carries a renewal number that is not greater, so it cannot extend the deadline. The reason: a late duplicate must not move the expiry beyond what the client last meant. After a client restart that lost its counter, the client reads the last number from inspect and continues above it.
- **The time is counted from receipt.** Network delay between the client sending a renewal and mvmd receiving it lengthens the deadline relative to the client's own clock; the client allows for that when it picks a length. The bound the client can rely on: because a renewal is acknowledged only after it is recorded, and recorded only after it is received, mvmd's expiry is never later than the moment the client receives the acknowledgement plus the length, measured on the client's monotonic clock. That is the figure the client compares with its own validity.
- **Renewal is acknowledged only after it is durably recorded.** The reason: once the client has an answer it relies on that expiry, and an expiry mvmd could lose in a restart would be a different one. The cost is a durable write per renewal.
- **Start and restore are refused while the deadline is not armed or has expired.** A Machine started without a deadline cannot be fenced. Pause, resume and the other calls are unaffected, and the Machines already running keep running until the deadline passes.
- **When the deadline passes, mvmd stops every Machine under it by force**, with no wait for the guest, because the guest cannot be trusted to cooperate and the client's own margin is already spent. Disks are retained. mvmd then starts nothing by itself; a later renewal re-arms the deadline and the client decides what runs ([runtime.md](./runtime.md) §Boundary with gaon).
- **Guarantee: the expiry is enforced whether or not the mvmd daemon process is running, and a VM runs at most δ past a recorded expiry.** δ is the time from the clock reaching the expiry to the end of the Machine's VMM process: the enforcing process waking, exiting, and the kernel killing the VMM ([ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)). It is finite and not yet a figure; it is measured with the single-node prototype ([runtime.md](./runtime.md) §Open questions). VMs survive an mvmd restart only until the recorded expiry (#2 §5 design principle 4). The reason: the deadline exists for the case where the client and mvmd's own process may both have died while the VMs live on, and a deadline that nothing counts then protects nothing. It is not weakened to "enforced once mvmd returns". A per-Machine supervisor provides it ([runtime.md](./runtime.md) §Fencing).
- **What the guarantee covers.** Failures of mvmd (a crash, a kill and a restart alike), of the enforcing process, and of the host (a reboot or a suspend). It does not cover a privileged host process that stops or traces the enforcing process while mvmd is down.
- **The client's margin covers δ.** A Machine can execute until mvmd's expiry plus δ, so the client keeps that sum, not the expiry alone, at or below its own validity.
- **Across an mvmd restart, a passed deadline is never extended.** The recorded expiry is held against a clock that keeps counting while mvmd is down and cannot be set, tied to the identity of the host's current boot, and not against the wall clock, which can be stepped backward. On start, before it serves any call, mvmd reattaches to its VMs ([runtime.md](./runtime.md) §State and stability) and compares: if the expiry has passed, it stops every Machine first; if not, the remaining time is what is left of the recorded expiry, never the full length. If the host has rebooted, no VM survived, and the deadline is treated as not armed.

## Rationale

- Contract rules: versioned interface, idempotency and defined state after failure (#2 §3 principle 5); request IDs on creating calls (#2 §5 design principle 3).
- Client-chosen identifiers and state-targeting calls are the two ways a retry becomes safe without a transaction: the first names the object before it exists, the second makes the repeat a no-op.
- The deadline's recorded-before-acknowledged rule and the receipt-time rule follow the safety requirement stated in garamsh/mvmd#4 Context: the client keeps the deadline at or below its own validity, which it can do only if mvmd's expiry is what the client was told.
- The mechanism that enforces the expiry while mvmd is not running: [ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md).
- The scope and why the API is not CRI: [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md).
- Why the policy is an order-free allow-list of prefix, protocol and port rules, and what it leaves for later: [ADR 0013](./adr/0013-a-network-policy-is-an-allow-list-of-address-protocol-and-port-rules.md).

## Open questions

- **A remote listener.** The API is served on a Unix domain socket on the node and nowhere else ([ADR 0004](./adr/0004-the-runtime-api-is-grpc-on-a-unix-socket-with-committed-generated-code.md)). Whether a network listener exists, and how it would authenticate, is undecided.
- **Running without a deadline.** Start and restore are refused unarmed; whether a standalone or development mode may opt out is undecided.
- **The unit of CPU and memory in Create Machine and Set resources.** The encoding is a vCPU count and a memory size in bytes. A change to that encoding follows the version rule under Scope and version.
- **The format of a patch in Patch file.** The encoding is an opaque byte string. A change to that encoding follows the version rule under Scope and version.
- **How the console is framed.** The encoding is a bidirectional stream of bytes, the Machine named in the first message. A change to that encoding follows the version rule under Scope and version.
- **The pattern syntax of List and search.** The encoding is a path and a pattern string with no syntax fixed, and the matches are returned as paths. A change to that encoding follows the version rule under Scope and version.
- **Authorisation of callers beyond the socket.** Authentication is the socket's filesystem permissions (ADR 0004); #2 states no finer rule.
- **How a disk other than a root disk comes to exist on the node** (creation, class, durability, movement): depends on #2 §6 item 9 ([runtime.md](./runtime.md) §Open questions). Attach assumes the node already holds it. A root disk is created by Create Machine and deleted by Delete Machine ([ADR 0009](./adr/0009-a-machines-root-disk-is-a-sparse-raw-file-with-one-holder.md)).
- **Changing a running Machine's policy.** A policy is fixed for the Machine's life in v1alpha1; a replace operation would be an addition.
- **Removal and retention of snapshots and images**, including garbage collection of the node cache.
- **How long events are kept**, how long request IDs are kept after their object is deleted, and how many API versions are served at once.
- **Whether a snapshot is a public resource** with its own list and inspect (#2 §6 item 8). Here a snapshot is created and used by identifier only.
