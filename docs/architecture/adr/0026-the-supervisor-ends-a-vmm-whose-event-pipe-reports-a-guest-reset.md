# ADR 0026: The supervisor ends a VMM whose event pipe reports a guest reset

> Status: accepted
> Date: 2026-11-10

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#155, found on the Cloud Hypervisor Driver (garamsh/mvmd#140). Under Firecracker a guest that panics or resets ends the VMM, because the guest's command line carries `reboot=k panic=1`: the panic resets the guest through the keyboard controller, and Firecracker exits on that reset. A Machine whose guest has panicked is then a Machine whose VMM has ended, which the Driver port already reports as `machine.ErrVMMEnded` (garamsh/mvmd#162). Cloud Hypervisor does not end.

**What the evidence is.** The source of Cloud Hypervisor at the v54.0 tag, read for this ADR:

- Every guest reset reaches the VMM through `reset_evt`: `VmExit::Reset` writes it (`vmm/src/cpu.rs` lines 1481 to 1485). The VMM's loop answers it with `vm_reboot()` and keeps running (`vmm/src/lib.rs` lines 2307 to 2312). `vm_reboot()` rebuilds the VM from the same configuration.
- Only `guest_exit_evt`, written for `VmExit::Shutdown` (`vmm/src/cpu.rs` lines 1487 to 1491), or `exit_evt` ends the VMM (`vmm/src/lib.rs` lines 2298 to 2305 and 2316 to 2325). A kernel panic with `panic=1`, which ends in a reset, never reaches either. Without `panic=` the guest halts with the VMM still running.
- The first thing `vm_reboot()` does is `event!("vm", "rebooting")` (`vmm/src/lib.rs` line 2777). The event monitor writes each event as indented JSON from `serde_json::to_string_pretty` (`event_monitor/src/lib.rs` line 125), so the line `"event": "rebooting"` is in the stream whatever caused the reset. The monitor takes `--event-monitor fd=<n>` (`cloud-hypervisor/src/main.rs` lines 315 to 320, 715) and makes that descriptor non-blocking (`event_monitor/src/lib.rs` lines 55 to 62).
- pvpanic is a device that only logs (`devices/src/pvpanic.rs`), and a guest that does not load a driver for it does not signal it.

So no command line makes Cloud Hypervisor end on a panic, and the one place a reset is visible from outside the VMM is its event monitor.

**What fits the design.** [ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md) makes the supervisor the init of the VMM's PID namespace and the process that runs while mvmd does not. [ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md) has the Driver describe the VMM process in VMM-neutral terms and the supervisor act on the description alone. This ADR extends what that description carries by one field; it does not rewrite ADR 0015.

Candidates weighed:

- **The supervisor reads a pipe the VMM writes its events to, and ends the VMM on a byte string the Driver names** — chosen.
- **An mvmd-side watcher with a `Reattach` operation.** mvmd would read the event stream and end the VMM itself. It does not run while mvmd is down, so a guest that panics while mvmd restarts leaves a VMM running that the Driver port calls ended, and it adds a Driver operation that every VMM written later must implement.
- **pvpanic alone.** At v54.0 it logs and ends nothing; the guest also has to load a driver for it, and a reset that is not a panic (a triple fault, a reboot) is not covered.
- **Guest poweroff.** The Guest Agent could power the guest off on its own failure, but a kernel that panics before PID 1 runs has no agent to do it, and the change would be to a component that is not this issue's.

## Decision

The description of the VMM process carries one more field: a descriptor number N and a byte string. An empty field means no pipe. The Firecracker Driver leaves it empty.

The supervisor creates a pipe and passes its write end to the VMM as descriptor N. It holds the read end and closes its own copy of the write end once the VMM has started, so the pipe ends with the VMM. It reads the pipe in pieces of at most 4096 bytes and matches the byte string across the boundaries of reads, carrying no more than the length of the string less one byte between them. On the first match it writes why to its log and kills the VMM with `SIGKILL`; it then reads on to the end of the pipe, so the VMM never blocks on it. The VMM's end follows the path every VMM's end takes: the supervisor drains the console, so that the panic text reaches `vmm.log`, and exits, and the Machine is a Machine whose VMM has ended. The supervisor refuses a request whose descriptor is below 7 (the ones under it are its own) or above 255, or whose string is empty or over 256 bytes.

The Cloud Hypervisor Driver passes `--event-monitor fd=7` and names the string `"event": "rebooting"`. Its kernel command line carries mvmd's guest parameters, as the Firecracker Driver's does: `reboot=k panic=1` on both architectures, and on x86_64 `i8042.noaux i8042.nomux i8042.nopnp i8042.dumbkbd`, which stop the kernel probing a keyboard and mouse the VMM does not have. `panic=1` is what turns a panic into the reset the event reports.

- **Why the VMM's end and not the supervisor's exit.** The fencing deadline exits the supervisor and takes the PID namespace with it, with no drain. Here the supervisor ends only the VMM and lets the path of a VMM's end drain the console, so that the panic text that preceded the reset reaches `vmm.log`, where the Machine's failure is read.
- **Why `SIGKILL` and not a request to the VMM.** A guest that has reset has nothing a graceful stop would save, and the supervisor has no channel to the VMM but signals.
- **The scenario that tests it.** A guest whose initrd holds no `/init` makes the kernel panic after it cannot mount a root, with no change to the Guest Agent.

## Consequences

- A guest panic or reset ends a Cloud Hypervisor VMM within the second `panic=1` waits and the supervisor's read, and the end is the one the Driver port reports as `ErrVMMEnded`. It holds while mvmd is not running, since the supervisor does it.
- The supervisor's code is VMM-neutral: a VMM written later that reports its end on a descriptor names its own number and string.
- A guest that resets on purpose cannot reboot in place under Cloud Hypervisor, which v54.0 would have let it do. A Machine's reboot is not an operation of the port.
- The supervisor trusts the stream as far as the VMM does: a VMM that a guest has escaped into can write the string to its own pipe and end only itself, which the guest could do by exiting.
- A match inside other output ends the VMM. The string is the Driver's and for Cloud Hypervisor is an event field the guest cannot write, since the pipe carries the VMM's events and not the console.
- A VMM that writes events faster than the supervisor reads finds the pipe full; the monitor's descriptor is non-blocking (`event_monitor/src/lib.rs` lines 55 to 62), so the VMM's write returns an error and does not wait. This ADR does not rely on what the monitor does with that error: the supervisor reads without waiting on anything.
- The i8042 parameters on Cloud Hypervisor's command line are copied from the Firecracker Driver's and not measured for Cloud Hypervisor; the real test boots with them.

## Signs the decision was wrong

- A guest that panics under `make test-cloud-hypervisor` leaves its VMM running past the bound, or a guest that does not panic has its VMM ended.
- A later Cloud Hypervisor changes the text of the event, or writes the reset event before the VM is rebuilt in a way that loses it: the match would stop ending anything, which only the real test shows.
- A VMM is admitted whose end is reported by something other than bytes on a descriptor, so the field serves one VMM.
- The `SIGKILL` is found to lose console output that a stop would have kept: the log would lack the end of the panic.
