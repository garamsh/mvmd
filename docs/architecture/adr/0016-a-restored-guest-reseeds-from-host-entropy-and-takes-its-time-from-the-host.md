# ADR 0016: A restored guest reseeds from host entropy and takes its time from the host

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#56. `guest.md` §Rationale names three things a snapshot restore does to a guest: its vsock connections are reset, its clock jumps, and clones restored from one snapshot share random state (garamsh/discussions#2 §2). [ADR 0011](./0011-each-machine-gets-a-routed-tap-and-addresses-mvmd-assigns.md) already has mvmd send the agent the Machine's new addresses after a restore, and [ADR 0014](./0014-the-guest-agent-serves-a-grpc-contract-it-owns-over-vsock.md) makes every call that changes state safe to repeat. This ADR decides what else that step carries and what the agent does with work that was in progress. The pinned VMMs are Firecracker v1.17.0 and Cloud Hypervisor v54.0 ([ADR 0010](./0010-a-machine-boots-a-pinned-kernel-and-the-guest-agent-initrd-from-raw-block-devices.md)). Everything below was read from their repositories at those tags and from Linux v6.12; a claim that could not be read there says so.

What the VMMs offer for randomness:

- **Firecracker v1.17.0.** `docs/snapshotting/snapshot-support.md` §Reusing snapshotted states securely: "During snapshot resume, Firecracker will update the 16-byte generation ID and inject a notification in the guest before resuming its vCPUs", and guests running Linux 5.18 or later "will re-seed their in-kernel PRNG upon snapshot resume". The same section: "State other than the guest kernel entropy pool, such as unique identifiers, cached random numbers, cryptographic tokens, etc **will** still be replicated across multiple microVMs resumed from the same snapshot." In `src/vmm/src/devices/acpi/vmgenid.rs`, `VmGenId::do_post_restore` triggers the interrupt and `make_genid` fills the identifier with `aws_lc_rs::rand::fill`. Firecracker "will always enable VMGenID" (`docs/snapshotting/random-for-clones.md`, §Linux kernels with VMGenID support).
- **The race.** The same document: "values returned by `getrandom()` and `/dev/(u)random` are distinct in all VMs started from the same snapshot, **after** the kernel handles the VMGenID notification. This leaves a race window between resuming vCPUs and Linux CSPRNG getting successfully re-seeded." Its §Recommendations: "On kernels starting from 5.18 onwards, the CSPRNG will be automatically reseeded when the guest kernel handles the VMGenID notification. To completely avoid the race condition, users should follow the same steps as with kernels < 5.18", which are an `RNDADDENTROPY` ioctl with bytes "generated locally in the guest, or obtained from the host" and then an `RNDRESEEDCRNG` ioctl, both needing `CAP_SYS_ADMIN`. It also says `virtio-rng` supplies entropy, "However, we cannot control when the guest kernel will request for random bytes from the device" (`docs/entropy.md` describes the device, which is optional).
- **Cloud Hypervisor v54.0.** `docs/snapshot_restore.md` §VM Generation ID: the device "is always present and needs no configuration"; the guest needs `CONFIG_VMGENID=y`; the driver "calls `add_vmfork_randomness()` when the value moves"; and "A program that seeded a generator of its own at startup carries that seed into every clone". In `vmm/src/vm.rs` the restore path calls `regenerate_vmgenid()` under the comment "Any restore may be a fork, so reseed before the guest runs", before `start_restored_vcpus()`.
- **What the guest kernel does with it,** from Linux v6.12: `drivers/virt/vmgenid.c` calls `add_vmfork_randomness(state->this_id, sizeof(state->this_id))`; `drivers/char/random.c` describes that function as adding the ID "without crediting, and then force-reseeds the crng". The `RNDADDENTROPY` ioctl there requires `CAP_SYS_ADMIN`, mixes the bytes with `write_pool_user` and calls `credit_init_bits`; `RNDRESEEDCRNG` requires `CAP_SYS_ADMIN`, returns `-ENODATA` while the generator is not ready, and otherwise calls `crng_reseed(NULL)`; a plain write to `/dev/urandom` only mixes (`random_write_iter` calls `write_pool_user`). The numbers are in `include/uapi/linux/random.h`: `RNDADDENTROPY` is `_IOW('R', 0x03, int[2])` and `RNDRESEEDCRNG` is `_IO('R', 0x07)`.

What they offer for time:

- **Firecracker v1.17.0.** `docs/snapshotting/snapshot-support.md`: a restored microVM "will be resumed with the guest OS wall-clock continuing from the moment of the snapshot creation. For this reason, the wall-clock should be updated to the current time, on the guest-side." On x86_64 with `kvm-clock`, a `clock_realtime: true` option on `LoadSnapshot` advances the clock, which "may cause issues within the guest as the clock will appear to suddenly jump". The VMClock device `VmClock::new` in `src/vmm/src/devices/acpi/vmclock.rs` is built with `clock_status: VMCLOCK_STATUS_UNKNOWN` and `counter_id: VMCLOCK_COUNTER_INVALID`, so it carries no time; it bumps `vm_generation_counter` and `disruption_marker` on restore, and the guest side of that needs a kernel with the patches the document names (merged in Linux 7.0, backported to Amazon Linux kernels).
- **Cloud Hypervisor v54.0.** `hypervisor/src/kvm/mod.rs`: `snapshot_clock` saves the kvmclock through `ClockData::with_realtime_filled` (`hypervisor/src/lib.rs`), which fills in the host's wall clock "so a later restore can advance the guest to current wall time", and `restore_clock` sets it before the vCPUs resume on x86_64. On aarch64 it advances the guest virtual counter to the current wall time itself, and "on `SnapshotRestore`/migration-receive it must catch up". `docs/snapshot_restore.md` says nothing about the guest's wall clock after a restore, so whether the guest's `CLOCK_REALTIME` is then right is not established by what I read.

What they say about work in progress:

- **Firecracker v1.17.0.** `docs/snapshotting/snapshot-support.md` §Vsock device reset: "On `SnapshotResume`, when the VM becomes active again, the vsock driver closes all existing connections. Existing listen sockets still remain active, but their CID is updated to reflect the current `guest_cid`."
- **Cloud Hypervisor v54.0.** `virtio-devices/src/vsock/device.rs` was searched for "reset", "transport", "snapshot" and "restore": it has the device `reset` and the `Snapshottable` implementation, and no transport-reset event on restore. Whether a connection survives a restore on Cloud Hypervisor is therefore not established.
- Processes, open files and the exec table are guest memory, which a snapshot holds whole; no source above says a VMM stops or changes them.

Candidates weighed:

- **Rely on VMGenID alone** for randomness. Both VMMs have it, but both leave the window the Firecracker document names, and a kernel without the driver (Firecracker: "In kernels where there is no VMGenID driver, the device will not have any effect") gets nothing.
- **A guest-side source of entropy** (`virtio-rng`, `RDRAND`). The documents above do not promise when either is used after a restore, and a guest that shares one generator state across clones cannot make a fresh draw by itself.
- **Entropy and time in a new call from mvmd, applied by the agent** — chosen. It is the step the Firecracker document recommends, it works the same on both VMMs, and it needs nothing a VMM may lack.
- **Putting them in `UpdateNetwork`.** The name would no longer describe the call, which garamsh/mvmd#56 asks an addition to keep.
- **Setting the clock only where a VMM leaves it wrong.** The agent cannot tell which VMM it runs on without naming one in the contract, and setting an already-right clock moves it by the delivery time of one call.
- **Stopping, killing or restarting commands at a restore.** The agent cannot know which work the client wants kept, and a command's effects are outside the agent ([runtime-api.md](../runtime-api.md) §Guest access: "mvmd never retries a command by itself").

## Decision

After a restore mvmd sends the agent one `CatchUp` call, and the agent has caught up when it returns.

- **The call.** `GuestService.CatchUp` in `guest-agent/proto/mvmd/guest/v1alpha1/guest.proto`, an addition that keeps v1alpha1. It carries a restore ID mvmd chose, from 32 to 256 bytes of entropy, the host's wall clock in nanoseconds since the Unix epoch, and the Machine's network. It names no VMM.
- **Randomness.** The agent writes the bytes into the kernel's pool with `RNDADDENTROPY`, counting every bit, then calls `RNDRESEEDCRNG`, so the generator is reseeded from them before the call returns. The VMM's VMGenID reseed comes first where there is one, and the agent's does not depend on it. mvmd draws the bytes from a cryptographically secure source, afresh for each restore; the same bytes sent for two restores would make the clones share state again. State a program keeps in user space is not the agent's to change.
- **Time.** The agent sets `CLOCK_REALTIME` to the time the call carries with `clock_settime`, whichever VMM it runs on. The error is the time one call takes to arrive. The monotonic clock is the VMM's.
- **Order and failure.** Entropy, then time, then network. A step that fails fails the call, naming the step, and the steps after it do not run.
- **Repeat.** The agent remembers the restore ID of the last call that succeeded, and a call with that ID succeeds and changes nothing, so a repeat after a lost answer cannot set the clock back. A call that failed or was dropped records nothing, and a repeat does every step again, each of which is safe twice. Two calls at once with one ID do the work once.
- **Work in progress.** The agent does nothing to it. A command that was running at the snapshot is running in the restored guest, with its record and its retained output as they were; a stream that was open is cut with its connection, and its client resumes it as ADR 0014 provides (output by offset, an upload that did not end left the old content, a patch guarded by its digests, a repeated `Exec` returning the same record). What a restored Machine's client does about a command it did not expect to find running is its own.
- **Refused before the boot.** `CatchUp` before `Boot` has succeeded, and a request with no network, an empty ID, entropy outside the sizes or a time that is not positive, is refused and changes nothing.

## Consequences

- The agent's reseed can run only after the guest does, so the window the Firecracker document names remains between the vCPUs resuming and the call being served. mvmd should send `CatchUp` before it lets any client work reach the guest.
- mvmd draws entropy for every restore, and a template snapshot restored many times costs one draw each.
- The agent needs `CAP_SYS_ADMIN` and `CAP_SYS_TIME`, which it has as PID 1, and the guest kernel needs the random pool's ioctls and `clock_settime`, which no configuration removes.
- A guest kernel gains `CONFIG_VMGENID` (with ACPI on x86_64) as a condition for the kernel's own reseed, which the agent's does not wait on.
- The guest's wall clock is right to within one call's latency after a restore on either VMM, and the monotonic clock is whatever the VMM leaves.
- Commands running at a snapshot run in every clone. A snapshot taken during a command's side effect repeats it in each clone if the command repeats it.
- Nothing in the contract or the agent changes with a third VMM.

## Signs the decision was wrong

- The window before `CatchUp` is served is long enough to matter, so reseeding has to happen in the guest before it runs, which would need a VMM-side mechanism.
- Cloud Hypervisor keeps vsock connections across a restore, or the agent's listener fails after one on either VMM, so the agent ends the Machine with `failed to serve the channel` after a restore; the agent would then need to survive an accept error.
- A clock set from a call is worse than the VMM's own correction on some VMM or CPU, for example when the call arrives late enough that the error matters to a workload.
- Clients need the agent to stop or kill work that was in progress at a snapshot, so the choice not to touch it was wrong.
- The kernel's reseed on `RNDRESEEDCRNG` is found not to take the credited bytes into the generator the way Linux v6.12 does, on a kernel the guest kernel project chooses.
