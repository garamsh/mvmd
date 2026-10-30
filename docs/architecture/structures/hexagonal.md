# Hexagonal Architecture

How a system partitioned into an application and the adapters around it is shaped: what a port is, what may reference what, and when a new port is earned.

## What it is

The system is divided into an inside and an outside. The inside is the application — the subject matter and the decisions made over it — and it names no technology it is reached through or reaches out to. The outside is adapters, one per technology, each translating between that technology and the inside.

A port is the boundary between the two: one conversation the application holds with the outside, declared in the application's own terms. A driving port is one an outside actor uses to put the application to work; a driven port is one the application uses to put something outside to work. Alistair Cockburn's statement of the pattern (*Hexagonal architecture*, 2005) names those sides primary and secondary after the use-case actors on each, and takes the hexagon for the room it leaves to draw ports rather than for a count of six.

Nothing here states how an adapter is built inside, or how the system is deployed. The first follows the technology it wraps; the second is a separate decision, and a hexagonal system is not required to ship as one unit.

## When it fits

- The same subject matter is reached more than one way — an interface, an API, a scheduled job — and none of them may grow its own copy of it.
- The technologies around the application change on a different clock than the subject matter: one is expected to be replaced or added to while the other holds.
- The tests that matter exercise the subject matter rather than the wiring, so the application is worth running with no technology attached.
- The subject matter is substantial. Where the application mostly forwards a request to a store, the inside is nearly empty and the ports cost more than they return.

## Boundaries and dependency direction

- **The port is the unit.** One port per conversation with the outside, named for that conversation and never for the technology serving it.
- **Dependencies point inward.** An adapter references the application; the application references no adapter. A technology's name inside the application — a framework type, a driver, a wire format, a table — is a defect, even where the language permits it.
- **Every crossing goes through a port.** A driving adapter reaches the application only through a port; the application reaches the outside only through a port an adapter implements. Something outside reached from the inside without a port is a defect even where the call compiles.
- **A port is declared in the application's terms.** Its operations and the types crossing it are the application's, and converting them to and from the technology's is the adapter's work. A type belonging to the technology in a port's signature puts the outside inside.
- **An adapter's only counterpart is its port.** One adapter calling another carries application work outside, where no port names it and no substitute stands in for it.
- **A new port is earned** by a conversation whose technology can be replaced without the application changing, and which the application can name without naming that technology. Cockburn's statement expects two or more adapters on a port — the real technology, and one for a test harness (*Hexagonal architecture*, 2005) — so a port with a single adapter that nothing substitutes is indirection.
- **Whether the inside is itself partitioned is contested.** Cockburn's statement leaves it undivided. Later readings built on it — Jeffrey Palermo's Onion Architecture (2008), and Robert C. Martin's *Clean Architecture* (2017), which names hexagonal among its sources — divide it into concentric rings with the subject matter innermost and every reference pointing inward. A project takes one, records which, and under the second a reference pointing outward from an inner ring is a defect.

## What it costs

- Every conversation is written twice — the port in the application's terms, the adapter's translation into the technology's — and a change to the conversation touches both.
- Following one request end to end means reading three places: the adapter, the port, and the application behind it.
- The isolation is only as good as the substitutes. What the real adapter does differently from the one the tests run against is found when the real adapter runs.
- A technology whose model does not fit the port's terms leaves the difference in the adapter — transactions spanning several ports, paging, batching, partial failure — and where the adapter cannot carry it, the port changes to suit the technology.
- The inside has no internal limit. This shape divides application from technology and nothing else, so subject matter accumulates in one inside as the system grows.

## Signs the choice was wrong

- A technology's vocabulary appears in the application's own signatures — a framework type, a column name, a status code.
- Adding or replacing a technology still changes the application.
- Most ports have one adapter, and the tests substitute none of them.
- The same rule is implemented in two adapters, and a fix has to be made in each.
- Ports are added and dropped with every feature, because they were drawn per use case rather than per conversation.
- Code cannot be found by subject matter, because everything the system decides sits in one undivided inside.
