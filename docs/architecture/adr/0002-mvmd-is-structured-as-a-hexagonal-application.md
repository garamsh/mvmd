# ADR 0002: mvmd is structured as a hexagonal application

> Status: accepted
> Date: 2026-11-08

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The candidates were hexagonal, layered and domain partitioning (the three files under `docs/architecture/structures/` at the bootstrap base commit 3ffb6e9, since deleted).

[garamsh/discussions#2](https://github.com/garamsh/discussions/discussions/2), adopted by [ADR 0001](./0001-mvmd-is-a-containerd-like-microvm-runtime-that-does-not-know-gaon.md), requires the VMM to be swappable behind a Driver interface with two implementations, Firecracker and Cloud Hypervisor, and Disk, Network, Snapshot Store and Image Store to be interfaces with one implementation each (#2 §5, first implementation scope). The Guest Agent is reached over vsock. Each of these is a conversation between the runtime's own subject matter (a Machine and its lifecycle) and a technology that is expected to change on a different clock. The runtime API is how the one client reaches the subject matter.

- **Hexagonal** fits: the same subject matter is reached through one API, the technologies around it are meant to be replaced or added to (the VMM above all), and the tests that matter, such as the fencing deadline and the one-writer-per-disk rule, exercise the subject matter with no VMM attached.
- **Layered** was rejected: the layers it separates (interface, service, persistence) are not the seam #2 draws, which runs between the subject matter and each technology, and a layer that only forwards is the layer #2 §3 principle 6 forbids.
- **Domain partitioning** was rejected: mvmd is one subject area (a node's Machines), so there are no sibling domains to partition between, and the VMM seam it leaves undefined is the one #2 cares about.

The structure leaves a point to the project, whether the inside is divided into rings (`docs/architecture/README.md` §Rules, "A point the structural rules leave to the project is settled in an ADR"). Dividing it adds a layer that owns no state and enforces no boundary the ports do not (#2 §3 principles 6 and 7).

## Decision

mvmd is a hexagonal application. The inside is the Machine lifecycle and the invariants over it, and it is not divided into rings. The runtime API is the driving port; the VMM Driver, Disk, Network, Snapshot Store, Image Store and the Guest Agent channel are driven ports. The boundary rules are in `docs/architecture/structure.md`.

## Consequences

- Every driven conversation is written twice, as a port in the inside's terms and an adapter's translation into the technology's, and a change to a conversation touches both.
- A port is earned by a technology that can be replaced, and a port with one real adapter earns its place through a test substitute that stands in for it (`docs/convention/stack-go.md` §1, the in-memory implementation). A port whose only adapter nothing substitutes is indirection and is removed.
- Tracing a request means reading the adapter, the port and the application behind it.
- The inside has no internal limit: Machine lifecycle, fencing, snapshot policy and image handling accumulate in it as mvmd grows. When that is felt, a later ADR divides it.
- The out-of-process Driver protocol of ADR 0001 is a new adapter on the existing Driver port and does not change the inside.

## Signs the decision was wrong

- A technology's vocabulary appears in the application's own signatures — a framework type, a column name, a status code.
- Adding or replacing a technology still changes the application.
- Most ports have one adapter, and the tests substitute none of them.
- The same rule is implemented in two adapters, and a fix has to be made in each.
- Ports are added and dropped with every feature, because they were drawn per use case rather than per conversation.
- Code cannot be found by subject matter, because everything the system decides sits in one undivided inside.
