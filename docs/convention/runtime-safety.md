# Runtime Safety

How types, trust boundaries, and error handling keep runtime errors out of production code.

## Contents
- Core principle
- Type system
- Trust boundaries
- Illegal states
- Total functions and exhaustive matching
- Errors reach a handler

## Core principle

Many runtime errors are bugs the type system could have caught. Catch those at compile time and parse time, so that what remains at runtime is the class no type can rule out — a machine out of memory, a network that stopped answering, an invariant nothing declared. Production code then does only what the types prove it can do.

## Type system

This section states what strictness and which type shapes are required, not the setting or syntax that provides them.

Enable the strictest mode your stack supports — strict nullability, no implicit escape to an "unknown/any/object" shape, no implicit conversions. Strictness catches more bugs at compile time than it costs in effort.

- **No silent assertions.** An assertion, cast, or lint suppression that does not check its own result silences the type system without fixing the underlying mismatch. Find the root cause. Where a language leaves no other way to read a value — out of an untyped container, for one — assert in the form that branches on failure; the unchecked form is still not accepted.
- **Non-nullable by default.** A type that allows absence everywhere spreads absence-handling through every caller. Mark absence explicitly (with an `Option` / `Maybe` / `Result` shape or a nullable wrapper) only where it is a real state — never as a default.
- **Domain primitives over raw types.** Define `UserId`, `Money`, `Email` as distinct named types (newtypes, branded types, value objects) rather than as raw strings or numbers. They prevent passing the wrong value at the type level.

## Trust boundaries

This section states where untrusted data is proved and what the boundary hands on, not the parser, validator or loader that does it.

External data is untrusted. The boundary between outside and inside is where you prove what's true.

- **Parse, don't validate.** A function that returns a parsed type (or throws at the boundary) is stronger than one that returns the raw input alongside a boolean validation result. After parse, downstream code trusts the type.
- **Validate once at the boundary** — HTTP request body, file read, env var, DB row, third-party API response. Don't re-validate at every call downstream.
- **The boundary returns a typed wrapper** so the rest of the codebase cannot accidentally pass the raw form.

## Illegal states

A type that allows `{ isPaid: true, isShipped: false }` and `{ isPaid: false, isShipped: true }` will eventually be misused. Encode state precisely.

- **Discriminated union / sum type** for a finite set of variants. The type system rules out impossible combinations (e.g., "shipped but unpaid" cannot be represented).
- **Avoid orthogonal boolean flags** as state — encode combinations as variants.
- **Make invalid combinations unrepresentable** — the compiler then prevents the runtime error.

## Total functions and exhaustive matching

Prefer functions that handle every case explicitly.

- **Exhaustive pattern matching** — when you add a variant, the compiler forces you to handle it (via a `never` / `unreachable` annotation or an exhaustive match).
- **An optional or sum return type is what lets a function be total** — the failing input has a value to return instead of a panic. Returning one does not make a function total by itself; it removes the reason it would not be.
- **Where the input space is small and finite, prefer total functions** to partial ones.

## Errors reach a handler

This section states which failure reaches which kind of place, not the form an error takes or the mechanism that maps it.

- **Expected failure** (validation, lookup miss, network error) is declared where it is produced and reaches exactly one place obliged to handle it. Where the language can force that, the return type is the place: failure sits in the signature, and the caller cannot reach the value without it. Where the language cannot — no checked exceptions — one declared boundary handler is the place, registered once for that failure rather than written as a `catch` at each call site.
- **Unexpected failure** (programmer bug, OOM, invariant violation): exceptions / panics are fine — they should crash visibly and be logged.
- **A failure that reaches no one is the defect.** A `try { ... } catch { /* ignore */ }` is one shape of it. A raised domain error that no handler maps is the other: it travels past every frame that knew what it meant and surfaces at whatever catch-all is outermost, as an unhandled failure rather than the specific one it was.
