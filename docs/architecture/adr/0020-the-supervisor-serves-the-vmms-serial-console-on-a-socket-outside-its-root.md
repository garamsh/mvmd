# ADR 0020: The supervisor serves the VMM's serial console on a socket outside its root

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#91, from the pull request that adds the Firecracker Driver (garamsh/mvmd#84, #30). At Firecracker v1.17.0 the guest's serial device is the VMM process's standard input and output. The supervisor gave the VMM no standard input (`/dev/null`) and its standard output went straight to `vmm.log` in the run directory, so `Driver.Console` could not open a session that writes to the guest, and the Driver did not report the capability `console` ([driver.md](../driver.md)).

The endpoint has to be somewhere a Driver can open and the VMM cannot reach. The run directory is the VMM's user's, read-write, and a VMM that has been escaped into can replace anything in it, including a socket mvmd connects to; [ADR 0015](./0015-the-supervisor-confines-every-vmm-process-the-same-way.md) keeps the supervisor's socket and log out of it for that reason. Two other constraints from earlier decisions: the VMM's root holds only the files its description names (ADR 0015, [ADR 0019](./0019-a-vmm-sees-its-machines-files-at-fixed-paths-in-its-root.md)), and the supervisor and mvmd are joined by a gRPC conversation on a Unix socket that carries no descriptor ([ADR 0003](./0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md)).

What was checked on the host the change was written on, Linux 6.12, with Firecracker v1.17.0 and the pinned Kata kernel: with pipes as the VMM's standard input and output, the guest boots, the VMM's output reaches the other end of the output pipe, and bytes written to the input pipe reach the guest's serial line, whose line discipline echoes them once a process holds `/dev/console` open (the Guest Agent does). `make test-firecracker` boots that and reads the echo.

Candidates weighed:

- **The supervisor holds two pipes as the VMM's standard input and output and serves a Unix socket of its own in the state directory** — chosen. The VMM holds only its ends of the pipes, which carry bytes and nothing else, and the socket is outside its root.
- **A socket in the run directory.** The VMM's user owns the directory and could replace the socket, so a Driver would connect to whatever the guest put there, and the daemon's `ConsoleLog` read of the same directory already needs `O_NOFOLLOW` against exactly that.
- **mvmd holds the pipes.** A descriptor cannot travel over the conversation (ADR 0003), the VMM is started by the supervisor and not by mvmd, and a restarted mvmd would lose the pipes of every running Machine.
- **A pseudo-terminal in place of the pipes.** The line discipline of the host's terminal would alter the guest's bytes and echo the input a second time; the console is a byte stream (runtime-api.md).
- **One socket pair as both standard input and standard output.** Firecracker makes its standard input non-blocking, and one open file description for both would make its output non-blocking too.

## Decision

The supervisor starts the VMM with its standard input the read end of one pipe and its standard output the write end of another (descriptors 5 and 6 of the confining child, which replace 0 and 1 after the request is read and are closed where they were passed). The standard error stays the log. The supervisor keeps the other ends and serves them on `console.sock` in the Machine's state directory, beside its own socket, created before the VMM starts and mode 0600 for the supervisor's user.

- **The output** is read to its end. Every byte is written to `vmm.log` first and then handed to each session open at that time; nothing is replayed to a session that opens later. `ConsoleLog` therefore reads the same file as before, with or without a session open, and the file is complete when the VMM has ended: the supervisor reads the pipe to its end before it exits.
- **A session** is one connection to the socket. What it sends is written to the VMM's input, in the order it arrives and merged with the other sessions'; what the guest writes is what it reads. It ends when it closes, when the VMM's output ends, or when it falls so far behind that its backlog (256 pieces of at most 4 KiB) is full. A session that reads nothing is dropped and never holds back the log, the other sessions or the VMM, which is never blocked on output.
- **The Firecracker Driver** reports `console`; its `Console` connects to `console.sock` beside the Machine's run directory, and an absent or refusing socket is "the console is not available", which is a VMM that has ended.
- **Protection.** Other users: the state directory is mode 0700 and the socket 0600, both the supervisor's. The VMM: the socket is outside its root, so it has no path to it, and what it holds is its two pipe ends, which carry bytes it could write to its own standard output anyway, since the guest can write the serial line. It gains no descriptor, mount or capability, and no step of ADR 0015's confinement changes or moves. Anything that can open the socket is mvmd or root on the node and can already read `vmm.log`.

## Consequences

- The supervisor, which enforces the fencing expiry, now also copies the guest's output while the VMM runs. It does not interpret a byte of it. The copy runs in goroutines apart from the expiry's timer thread, which is unchanged, and a console that is slow or stuck cannot delay the expiry; the memory it can hold is the backlog of each open session. The supervisor's own text no longer says it holds nothing of the inside; it holds the VMM's serial bytes in transit.
- When the VMM ends the supervisor waits up to two seconds for the output to reach the end of its pipe before it exits, so a descendant of the VMM that kept the pipe open delays the exit by that long and the log may then lack its last bytes.
- A VMM whose serial device is a socket or a file serves the same two Driver operations by another wiring; a VMM with no serial device on its standard input and output gets nothing from this and does not report the capability.
- Writes from several sessions are not separated: two sessions typing are one typist to the guest.

## Signs the decision was wrong

- A VMM admitted later takes its serial from a path or a descriptor other than standard input and output, so that wiring them is no longer the general case.
- The output's rate makes the supervisor's copy a measurable cost to a Machine's fencing, or its memory a cost to the node.
- A use of the console needs to reach the output from before its session opened, or input kept apart per session.

## Errata

- 2026-11-09: the choice of two pipes as the VMM's standard input and output is superseded by [ADR 0025](./0025-the-supervisor-gives-the-vmm-a-raw-mode-pseudo-terminal-as-its-console.md), which gives the VMM a raw-mode pseudo-terminal in their place. The rest of this decision stands.
