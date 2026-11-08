# ADR 0004: The runtime API is gRPC on a Unix socket, with committed generated code

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

[runtime-api.md](../runtime-api.md) fixed the operations and their meaning and left the wire encoding and transport open. The first code pull request that creates `lib/runtimeapi/` had to settle them. The project owner's goal of 2026-11-08 asked for long-term, general decisions rather than short-term workarounds.

Forces:

- The client, gaon's Node Agent, runs on the same node as mvmd, as containerd's client does.
- The Driver contract is already shaped for gRPC (discussions#2 §5 design principle 5), and a guest channel is to come, so one RPC stack can serve all three.
- The version rule in runtime-api.md §Scope and version needs an encoding with a defined compatibility discipline.
- Events, console, exec output and file transfer are streams.
- gaon imports `lib/runtimeapi/` with `go get`, which runs no generator.

## Decision

- **Wire and transport.** The runtime API is a gRPC service defined in Protocol Buffers, served on a Unix domain socket on the node.
- **Authentication.** The socket's filesystem permissions: only the owning user and group may connect. There is no network listener.
- **Version in the package.** The protobuf package and the Go package path carry the version (`mvmd.runtime.v1alpha1`, `lib/runtimeapi/v1alpha1`). A breaking change is a new package beside it.
- **Generated code is committed.** The `.proto` files are the authored source; the generated Go is committed, and a check fails when regenerating changes any committed file.
- **Generator pinned in the module system.** `buf` and the two Go plugins are Go programs, pinned as `tool` directives in `lib/runtimeapi/tools.mod` with checksums in `tools.sum`, apart from the library's `go.mod` so that a consumer's dependency graph does not carry the generator's.

## Consequences

- A node-local client needs no certificates, and the socket's mode is the whole access rule.
- Anything other than the owning user or group is refused by the kernel before a byte is read; a finer-grained rule than that needs a new decision.
- A remote client cannot connect. A remote listener is a later decision and is listed under runtime-api.md §Open questions.
- The library module's `go` directive follows gRPC's (1.26.0 at the pinned version), so a consumer builds with at least that Go.
- A change to a `.proto` file is two files in the diff, the source and its output, and the pipeline fails when they disagree.
- The generator module's `go` directive follows buf's (1.26.7), so the entry points set a toolchain floor of that version.

## Signs the decision was wrong

- A client that has to run off the node and cannot be given a socket forwarded to it.
- A need for per-caller authorisation finer than the socket's owner and group.
- Generated output differing between machines running the pinned generator, so that the committed-output check fails without a source change.
- The consumer's build rejecting the library for the Go version its `go` directive requires.
