# ADR 0025: The supervisor gives the VMM a raw-mode pseudo-terminal as its console

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#147, found on garamsh/mvmd#140 (the Cloud Hypervisor Driver). This ADR supersedes the choice in [ADR 0020](./0020-the-supervisor-serves-the-vmms-serial-console-on-a-socket-outside-its-root.md) of two pipes as the VMM's standard input and output, and keeps the rest of it: the socket outside the root, the log written first, the sessions, the backlog and the protection.

**What the evidence is.** At Cloud Hypervisor v54.0, `vmm/src/serial_manager.rs` lines 129 to 132 (`SerialManager::new`) accepts serial input in `--serial tty` mode only if `libc::isatty(libc::STDIN_FILENO) == 1`. The PM read the file at tag v54.0 and recorded it in garamsh/mvmd#147; it was not read again for this ADR. With a pipe as its standard input the guest's output still arrives and the input is dropped, so the `drivertest` console scenario cannot pass on Cloud Hypervisor. It is the first of the "Signs the decision was wrong" of ADR 0020: a VMM admitted later takes its serial from standard input only when that is a terminal.

**Why ADR 0020 rejected a terminal, and what answers it.** It rejected one because the line discipline of the host's terminal would alter the guest's bytes and echo the input a second time. A terminal in raw mode (`cfmakeraw`: no echo, no translation of input or output, no signals, no line buffering) passes bytes unchanged, and the guest's own line discipline does the echo, as it does with pipes. The supervisor's tests read this on Linux 6.12.115: the bytes `\r`, `\n`, `0x03` and `0x7f` cross in both directions unchanged, and nothing is echoed to the session.

**Why one descriptor cannot be both input and output.** ADR 0020 rejected one socket pair for both because the VMM makes its standard input non-blocking. Cloud Hypervisor does the same (`fcntl` with `O_NONBLOCK` on a `dup` of its standard input, as recorded in #147), and `dup` shares the open file description. A terminal's subordinate side is opened by path, so each open is its own description: the VMM's standard input and output are two opens, and making one non-blocking leaves the other blocking.

Candidates weighed:

- **A raw-mode pseudo-terminal, two opens of its subordinate side as standard input and output** — chosen.
- **Cloud Hypervisor's own `--serial socket=` in the run directory.** ADR 0020 rejects a socket in the run directory: the VMM's user owns it and can replace it.
- **Pipes for a VMM that does not need a terminal and a terminal for one that does.** Two wirings in the supervisor and in the suite for one capability, where the terminal serves both.
- **A terminal as the VMM's controlling terminal.** The VMM would then be in the terminal's session and signals and hang-ups of the terminal would reach it; nothing it does needs that.

## Decision

The supervisor opens a pseudo-terminal pair, sets it to raw mode, and opens the subordinate side twice, without making it any process's controlling terminal (`O_NOCTTY`; no `TIOCSCTTY`; the VMM is in no session of the terminal). The confining child receives the two opens as descriptors 5 and 6, which replace its standard input and output after the request is read, as ADR 0020's pipes did; the standard error stays the log. The supervisor keeps the controlling side and serves it on `console.sock` as ADR 0020's Decision describes the pipes: the log first, then the sessions, with the same backlog and the same bound on the wait for the end of the output.

- **End of output.** The controlling side reports `EIO` once the last subordinate descriptor has closed and what was written has been read. That is the end of the output and not an error: the supervisor reads the output to its end, `vmm.log` is complete, and the sessions end.
- **Confinement.** The VMM holds its two descriptors and nothing else of the terminal. No `/dev/pts` and no `/dev/ptmx` is mounted or created in its root, and no step of [ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md)'s confinement changes. The supervisor closes its own copies of the subordinate side once the VMM has started, since the output cannot end while one is open.
- **The suite.** `daemon/internal/drivertest/` starts the VMM through the function the supervisor package exports for this (`OpenConsole`), so the wiring the suite plays is the supervisor's and is not copied.
- **The Drivers.** No Driver operation, capability or file changes: `console.sock`, `vmm.log` and the two console operations of [driver.md](../driver.md) are as ADR 0020 made them.

## Consequences

- Every VMM the Driver port admits can take console input, whether it reads a descriptor or insists on a terminal.
- The supervisor needs `/dev/ptmx` and a `devpts` mount in its own mount namespace, the host's. It has them where it runs today; a supervisor started with neither fails the start of the VMM.
- The VMM can change the terminal's settings through its descriptors, for example turn echo on. That alters the bytes of its own console and nothing outside it, since the guest could already write any byte to its serial line. The terminal has no window size; a VMM that needs one gets none.
- The end of the output is `EIO` from the controlling side where it was end of file from a pipe, so the supervisor's read translates it, once, where the terminal is read.
- A descendant of the VMM that keeps a subordinate descriptor open keeps the output from ending, as with a pipe; the wait of two seconds before the supervisor exits and the possibly incomplete log in that case are as ADR 0020 states.
- ADR 0020's rejection of a terminal is answered, not repeated: a later change that returns to pipes loses Cloud Hypervisor's input.

## Signs the decision was wrong

- A VMM admitted later alters or loses a byte of its console under a raw-mode terminal that it passes through a pipe, or needs a window size or a controlling terminal.
- A log is found to lack the last bytes of a VMM that had ended, with the supervisor having read the controlling side to `EIO`: the terminal would then lose output that a pipe kept.
- Cloud Hypervisor, or another VMM, is changed to take its serial input without a terminal, so that the terminal serves nobody that a pipe did not.
- A kernel or a mount namespace without `/dev/ptmx` has to be supported for the supervisor.
