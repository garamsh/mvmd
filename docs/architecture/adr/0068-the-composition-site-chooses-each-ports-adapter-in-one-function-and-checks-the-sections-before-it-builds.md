# ADR 0068: the composition site chooses each port's adapter in one function and checks the sections before it builds

> Status: accepted
> Date: 2026-11-23

Append-only: once merged, the body below is not rewritten. A fact later found wrong is corrected in an appended note, not edited out; a revised decision is a new ADR that supersedes this one.

## Context

The architecture scan of garamsh/mvmd#784 named the composition site's choice, per port, between an in-memory substitute and a real adapter (candidate A4). `daemon/internal/app/app.go` builds every substitute, then replaces each by the section of the configuration that selects the real one. Six error exits each closed the database by hand, and which combinations of sections are valid is checked in `config.go`, apart from where the sections are used. The scan tied the shape to no bug.

## Decision

The shape stays and the exits change.

- **The choice stays in one function.** `build` makes each port's adapter from the sections of the configuration, in order, so a reader sees every port's adapter in one place. A port chosen by a function of its own would put the choice for the ports that share a section, as the Drivers share the supervision section, in several places, and the order the adapters need to be built in would be held by their callers.
- **The sections are checked before anything is built.** `Config.check` states which combinations are valid, once, when the configuration is read. A node is refused for a combination before its database is open, and no adapter checks its own section again where it is built. Moving the checks to where the sections are used would run them after the database is open, which is the failure the exits were repeating a line to answer.
- **The database is closed by one deferred call.** The database stays open only for an `App` that is returned, so an exit of `build` closes it without saying so, and an exit added later closes it too.

## Consequences

- A new section adds a case to `Config.check` and a step to `build`, and the step needs no line to close the database.
- `build` stays a long function; its length is the number of ports, and each step is the same shape.

## Signs the decision was wrong

- A combination of sections is refused only after a step has begun, because what makes it invalid is known only there.
- A port has more than one real adapter selected by something other than the Driver, and the choice needs a branch that `build` cannot hold in line.
