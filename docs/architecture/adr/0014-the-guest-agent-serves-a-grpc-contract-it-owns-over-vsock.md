# ADR 0014: The Guest Agent serves a gRPC contract it owns over vsock

> Status: accepted
> Date: 2026-11-09

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

Raised as garamsh/mvmd#11. The Guest Agent serves exec and the file API to mvmd inside each Machine, over vsock ([guest.md](../guest.md); `runtime-api.md` §Guest access). The decision-change reply of garamsh/discussions#2 (discussioncomment-19466961) left the encoding of that channel open.

Forces:

- The runtime API is gRPC ([ADR 0004](./0004-the-runtime-api-is-grpc-on-a-unix-socket-with-committed-generated-code.md)), and the Driver protocol to come is shaped for it (#2 §5, design principle 5), so one RPC stack serves the project.
- Exec output and file content are streams, and no file is bounded by one message (`runtime-api.md` §Guest access).
- Restoring a snapshot resets vsock connections (`guest.md` §Rationale), so the daemon reconnects and every call that changes state must be safe to repeat.
- The daemon and a guest image built earlier can differ by a version.
- One unit reaches another only through what it publishes (`structure.md` §Repository layout), and the guest is Rust while the daemon is Go.

Candidates weighed:

- **A gRPC service in Protocol Buffers, defined in the Guest Agent's tree** — chosen.
- **A hand-framed protocol over vsock.** It would need its own streams, its own compatibility rules and a codec in each language, for what gRPC already provides.
- **The contract in the daemon's tree.** The guest serves the contract and is the side that must keep older images working, so it owns the contract; the daemon is a client of it.
- **Committing the generated Rust, as `lib/runtimeapi` commits its Go.** That library is imported by gaon; no other project imports the guest's generated code, so there is no consumer a committed copy would serve.
- **Compiling the `.proto` with a `protoc` binary.** It adds a binary the build must find, where `protox` is a Rust parser pinned by `Cargo.lock`.

## Decision

- **Wire format.** The Guest Agent serves a gRPC service, `mvmd.guest.v1alpha1.GuestService`, on vsock. The package carries the version, by the rule of the runtime API.
- **The contract's home.** `guest-agent/proto/mvmd/guest/v1alpha1/`. The daemon generates its own Go client from that file with the `buf` pinned in `tooling/protogen/` (ADR 0004), and reaches the guest tree only through the file. `buf lint` covers `guest-agent/proto/` too.
- **Rust code is generated at build time**, by `tonic-prost-build` with `protox` as the compiler, and is not committed.
- **Every call that changes state is safe to repeat.** Exec carries the client's exec ID: a repeated start with the same ID returns the same record and never runs the command twice. Write is whole old or whole new content, and patch is guarded by a base and an expected digest (`runtime-api.md` §Guest access).
- **Exec output is kept by the agent** for the life of the agent, so a read resumes after any byte offset.
- **The listener is a seam.** The server takes any stream of incoming connections; the binary passes vsock, and tests pass a Unix socket in a temporary directory through the same server code.
- **Console is not in the contract.** It is the VMM's serial device.

## Consequences

- The daemon's Guest Agent channel adapter is written against the `.proto` and needs no VM to be tested, since the server runs over a Unix socket.
- A contract change is a change to one file, checked by `buf lint`; protobuf compatibility rules let the daemon and an older guest differ by a version.
- The guest binary carries a gRPC stack, which adds to its size and to the initrd.
- Exec output held in the agent's memory lasts as long as the agent; a Machine restored from a snapshot continues with the agent's memory as it was.
- The build needs no `protoc`, and generated Rust is never stale since it is never committed.

## Signs the decision was wrong

- The gRPC stack's size or start time makes the Guest Agent's boot slower than a hand-framed protocol would measurably be.
- A second project imports the guest's generated Rust, so the reason for not committing it no longer holds.
- Exec output held in memory exhausts a guest's memory for long-running commands, and the output has to move to disk or be bounded.
- Old guest images cannot serve a daemon one version newer, so the compatibility rules did not carry what this decision relies on them for.
