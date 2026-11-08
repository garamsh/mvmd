# Structure

The shape mvmd is partitioned into, what may reference what, how the repository is laid out, and when a new port is earned.

Checked against garamsh/discussions#2 as read on 2026-11-08 (decision-change reply discussioncomment-19466961) and against garamsh/garam at commit 743f7b390ac60cb18ae3ba6e867237a57fe8cc8f, `docs/architecture/structure.md` §Services. Claims about #2 hold for those; they promise nothing about later edits of the discussion.

## The shape

mvmd is a hexagonal application ([ADR 0002](./adr/0002-mvmd-is-structured-as-a-hexagonal-application.md)). The inside is the Machine lifecycle on one node and the invariants over it; it names no VMM, disk, network, registry or wire format. Adapters translate between the inside and each technology.

The inside is not divided into rings. The Go package layout of `docs/convention/stack-go.md` §1 is file placement and adds no direction rule between packages inside it.

Nothing here states how an adapter is built inside, or how mvmd is deployed.

## Repository layout

Follows the monorepo shape of garamsh/garam at the commit above: each top-level folder is one responsibility, and one unit reaches another only through what that unit publishes. A folder is created by the pull request that first adds code to it, and not before; none of the code folders below exists yet.

- `daemon/` — the mvmd daemon: a Go module in the service layout of `docs/convention/stack-go.md` §1, binary at `daemon/cmd/mvmd/main.go`. It holds the inside, the driving adapter for the runtime API, and every driven adapter, including the in-process VMM Drivers.
- `daemon/cmd/mvmd-supervisor/main.go` — the per-Machine supervisor that enforces the fencing expiry ([runtime.md](./runtime.md) §Fencing), a second binary of the daemon module. It holds no part of the inside, and its `main.go` wires only itself; the application's one composition site stays with `daemon/cmd/mvmd/main.go`.
- `guest-agent/` — the Guest Agent as its own Cargo workspace (#2 §5, implementation language). Its Rust code follows `docs/convention/stack-rust.md`.
- `lib/runtimeapi/` — the runtime API contract and its client, a Go library module in the library layout of `docs/convention/stack-go.md` §1, package at the module root. It is the one module gaon imports. It is a leaf: it imports no other package of mvmd.
- `mvmctl/` and `imageconv/` — the names reserved for a CLI and for the isolated image converter (#2 §5, image section, security), each a top-level folder if and when it ships as its own executable. Neither is committed to existing.
- `docs/` — conventions, architecture, procedures.
- `tooling/protogen/` — a Go module that holds nothing but the pins of the protobuf generator (`buf`, `protoc-gen-go`, `protoc-gen-go-grpc`) as `tool` directives, released versions only, with `protoc-gen-go` at the version of the library's `google.golang.org/protobuf`. It ships no package and is not in `go.work`, so the generator's dependencies stay out of every unit's graph.
- `deploy/` — added later, by the first pull request that needs one.
- `go.work` at the root, naming every Go module that ships code and carrying the one `toolchain` line every check and the pipeline use. The root holds no Go package.
- `lib/` holds libraries linked by more than one unit and no binary. It has no `shared/`, `utils/` or `common/`; a package one unit alone imports belongs to that unit.
- A unit reaches another only through what it publishes: Go's `internal/` encloses `daemon/` and keeps other modules out of it. The daemon's inside does not import `lib/runtimeapi/`; the driving adapter does, and converts between its types and the driving port's.

## Boundaries and dependency direction

- **The port is the unit.** One port per conversation with the outside, named for that conversation and never for the technology serving it.
- **Dependencies point inward.** An adapter references the application; the application references no adapter. A technology's name inside the application — a framework type, a driver, a wire format, a table — is a defect, even where the language permits it.
- **Every crossing goes through a port.** A driving adapter reaches the application only through a port; the application reaches the outside only through a port an adapter implements. Something outside reached from the inside without a port is a defect even where the call compiles.
- **A port is declared in the application's terms.** Its operations and the types crossing it are the application's, and converting them to and from the technology's is the adapter's work. A type belonging to the technology in a port's signature puts the outside inside.
- **An adapter's only counterpart is its port.** One adapter calling another carries application work outside, where no port names it and no substitute stands in for it.
- **A new port is earned** by a conversation whose technology can be replaced without the application changing, and which the application can name without naming that technology. Cockburn's statement expects two or more adapters on a port — the real technology, and one for a test harness (*Hexagonal architecture*, 2005) — so a port with a single adapter that nothing substitutes is indirection.
- **Whether the inside is itself partitioned is contested.** Cockburn's statement leaves it undivided. Later readings built on it — Jeffrey Palermo's Onion Architecture (2008), and Robert C. Martin's *Clean Architecture* (2017), which names hexagonal among its sources — divide it into concentric rings with the subject matter innermost and every reference pointing inward. A project takes one, records which, and under the second a reference pointing outward from an inner ring is a defect.

Here the last bullet is taken as follows: the inside is not divided into rings, so no reference inside it is a defect for its direction.

## Shape rules every project carries

- **The application has one composition site.** It is the one place that constructs concrete implementations, and it decides which implementation each declared abstraction receives.
- **Every other unit receives what it depends on and constructs none of it**, whatever the shape makes the unit — a domain, a layer, or an adapter.
- **The composition site stays thin.** It constructs and passes; it decides nothing else.

In mvmd the composition site is in the daemon, placed as `docs/convention/stack-go.md` §1 "Project envelope" places it.

## Ports

| Port | Kind | Counterpart | Basis |
|---|---|---|---|
| Runtime API | driving | gaon's Node Agent, through `lib/runtimeapi/` | #2 decision-change reply; [runtime.md](./runtime.md) |
| VMM Driver | driven | Firecracker, Cloud Hypervisor; later an in-house VMM | #2 §5, VMM Driver; [driver.md](./driver.md) |
| Disk | driven | one implementation at first | #2 §5, first implementation scope |
| Network | driven | one implementation at first | #2 §5, first implementation scope |
| Snapshot Store | driven | one implementation at first | #2 §5, first implementation scope |
| Image Store | driven | one implementation at first; [image.md](./image.md) | #2 §5, first implementation scope |
| Guest Agent channel | driven | the Guest Agent over vsock; [guest.md](./guest.md) | #2 §3 wiring, §5 |
| Supervision | driven | `mvmd-supervisor` processes, one per Machine whose VMM process exists; [runtime.md](./runtime.md) §Fencing | [ADR 0003](./adr/0003-a-per-machine-supervisor-enforces-the-fencing-expiry.md) |

A port with one real adapter is kept only with a test substitute standing in for it ([ADR 0002](./adr/0002-mvmd-is-structured-as-a-hexagonal-application.md) §Consequences). The Driver port has two real adapters from the start, since an abstraction with one implementation cannot be verified (#2 §5, first implementation scope).

## Rationale

- Why this shape: [ADR 0002](./adr/0002-mvmd-is-structured-as-a-hexagonal-application.md).
- Why mvmd is a leaf that gaon depends on: [ADR 0001](./adr/0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md); the principle is #2 §3 principle 3 (dependency direction is one-way).
- Why no layer that only forwards: #2 §3 principle 6; why few concepts: principle 7.

## Open questions

- Whether mvmd's own durable state (#2 §3, the node as the reference for actual state) is a driven port with a substitute, or an adapter detail of the inside. #2 does not name it among the interfaces of §5.
- The names of the inside's domain packages; the first code pull request chooses them under `docs/convention/stack-go.md` §1.
