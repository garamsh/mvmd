# Rust — Layout, Errors and Verification Conventions

How a Rust Cargo workspace is laid out, where its mechanisms are declared, and the commands behind the entry-point names.

Checked against rustc, cargo and clippy 1.99.1 (58cf8f9c35, 2026-10-13), rustfmt 1.10.0, cargo-deny 0.20.2 and thiserror 2.0.21, on x86_64 Linux. A claim below that names no version holds for these. A claim marked "measured" was run against that toolchain in a scratch workspace; one without the mark is the documented behaviour of the tool.

## Contents
- Workspace and crate layout
- Naming and files
- Toolchain
- Lints
- Unsafe code
- Error handling
- Comments
- Dependencies
- Testing
- Verification commands

## Workspace and crate layout

- The workspace root is the directory holding a `Cargo.toml` with `[workspace]` and no `[package]` (a virtual manifest). It also holds `Cargo.lock`, `rust-toolchain.toml`, `clippy.toml` and `deny.toml`. Every command below runs from it.
- A virtual manifest names `resolver = "3"` itself. Edition 2024 does not supply the resolver for a manifest with no package.
- Members live at `crates/<name>/` and are listed by the glob `members = ["crates/*"]`. Shared package keys (`edition`, `rust-version`, `publish`) sit in `[workspace.package]`, and each member inherits them with `key.workspace = true`. Every crate is edition 2024 and `publish = false`.
- **A crate that ships a binary is a binary crate; one that exists for other crates is a library crate.** One crate may hold both targets (`src/main.rs` and `src/lib.rs`). Start with one crate; a second is added when a second consumer of its code exists.
- **The composition site is the binary's `main.rs`, unless a test outside it has to reach it.** An integration test links the crate's library target and nothing in `main.rs`, so where such a test exercises the wired program, the site is a function in `lib.rs` that `main` calls and `main` holds nothing else.
- The shipped binary is built for `<arch>-unknown-linux-musl`, which links statically (measured on x86_64: `file` reports `static-pie linked`). `rust-toolchain.toml` lists each such target in `targets`, so the pinned toolchain installs its standard library with no further step.
- Profile keys sit in the root manifest only, where Cargo reads them. The `release` profile sets `lto = true`, `codegen-units = 1`, `strip = true` and `panic = "abort"`, to keep the binary small: on a one-file program the musl build measured 545800 bytes without the four and 385720 with them. Tests, benchmarks and build scripts ignore `panic`, so a test that asserts a panic still works.

## Naming and files

This section states the Rust spelling only; casing is the compiler's own `non_snake_case` and `non_camel_case_types` lints, which the lint entry point fails on.

- Package names in manifests are kebab-case (`guest-agent`); the same name in code is snake_case (`guest_agent`).
- Source files are snake_case and named for a concept. A module is `name.rs`, with its children in `name/`. **`mod.rs` is banned**, by `clippy::mod_module_files` (measured: a `mod.rs` fails the lint entry point, the same module as `m.rs` passes).
- Test functions are snake_case sentences naming the behaviour (`first_reads_the_first_byte`).

## Toolchain

- `rust-toolchain.toml` at the workspace root pins one exact release (`channel = "1.99.1"` form, never `stable`), with `components = ["clippy", "rustfmt"]` and the musl `targets`. `rustup` picks it up for every `cargo` and `rustc` call under the directory (measured: `rustc --version` reports 1.99.1 inside, 1.98.0 outside), and installs it on first use. A channel name would move the compiler on a day nobody changed the code.
- Each manifest's `rust-version` equals the pin, via `[workspace.package]`.
- Moving the pin is an ordinary change. It re-reads the release's documentation, updates this file's first paragraph, and re-measures the claims marked "measured".
- No `rustfmt.toml`: the defaults are the format.

## Lints

- Levels live in the root manifest's `[workspace.lints]`, and every member carries `[lints] workspace = true`. Cargo rejects any other lint key beside it (measured: `cannot override workspace.lints in lints`), so a crate cannot set its own level.
  - A member added without the stanza falls back to the compiler's defaults and nothing reports it. A review looks for the stanza in each new `Cargo.toml`.
- `clippy::all` is denied, with `priority = -1` so the single lints named beside it win over the group. `clippy::restriction` is never enabled as a group; its members contradict each other. Members are named one at a time:
  - `unwrap_used`, `expect_used`, `todo`, `unimplemented`, `dbg_macro` (§Error handling).
  - `undocumented_unsafe_blocks` (§Unsafe code).
  - `mod_module_files` (§Naming and files).
  - `allow_attributes` and `allow_attributes_without_reason`: a suppression is `#[expect(lint, reason = "...")]`, never `#[allow]`. An `expect` that stops being needed fails the lint entry point (measured: `this lint expectation is unfulfilled`).
- `clippy.toml` at the workspace root sets `allow-unwrap-in-tests = true` and `allow-expect-in-tests = true`. Measured: with them, `unwrap()` in a `#[cfg(test)]` module and in a `tests/` file passes; without `allow-unwrap-in-tests`, the module fails.
- Cargo reads a `RUSTFLAGS` that carries `--cap-lints allow`, and the denied lints then pass (measured: a denied `unwrap` compiled clean under it). The entry points unset `RUSTFLAGS` and `CARGO_ENCODED_RUSTFLAGS`, so a check reads the levels this repository states. A `cargo` call made outside an entry point, in a shell that exports either, is unprotected.

## Unsafe code

- `unsafe_code = "deny"` in `[workspace.lints.rust]`. It is denied and not forbidden: a forbidden lint cannot be lifted by any attribute, so the first genuinely necessary use would need a change to the whole workspace's level, where a deny lets the one item that needs it carry the reason.
- An item that needs `unsafe` carries `#[expect(unsafe_code, reason = "...")]` on the narrowest item, and every `unsafe` block under it opens with a `// SAFETY:` comment. `clippy::undocumented_unsafe_blocks` fails the block without one (measured). `unsafe_op_in_unsafe_fn`, warn by default in edition 2024, requires a block inside an `unsafe fn` too.
- A system call that a dependency exposes safely is called through it. `unsafe` is for what no safe wrapper provides.
- Does not catch: dependencies. Cargo compiles a registry dependency with lints capped, so this is a statement about this repository's code.

## Error handling

- **Expected failure is a `Result`.** The error type is an enum, one variant per failure a caller branches on, with its data in the variant; the derive is `thiserror`'s `Error`. A caller that does not branch receives an opaque struct wrapping a private enum, so the variants are not API.
- **A dependency's error type appears in no public signature.** An adapter translates it into the crate's own enum before it returns; `#[from]` and `#[source]` set `Error::source()`, which publishes the cause to any caller that walks it, so they are used where that is meant and not otherwise.
- Context is added at a boundary — IO, an external call, a system call — as a variant or field, not on every line.
- **A gRPC status is built in one place**: the module implementing the generated service. No other module names a status code.
- **Return, and let the outermost boundary report once.** A function does not log an error it also returns.
- `unwrap`, `expect`, `todo!`, `unimplemented!` and `dbg!` are denied outside tests (measured: each fails the lint entry point). A violated invariant is stated with `assert!` or `unreachable!`, which panics and ends the program; no code recovers from a panic, and `panic = "abort"` in `release` makes that so.

## Comments

- A crate or module is described by an inner doc comment (`//!`) on its first line and every `pub` item of a library crate carries an outer one (`///`).
- The one marker is `// TODO(name): …`. `todo!()` is a panic and is denied (§Error handling).
- A `// SAFETY:` comment states why the block upholds the invariants of what it calls (§Unsafe code).

## Dependencies

- `Cargo.lock` is committed. Cargo records a checksum for each registry package in it and verifies the download against it, so the lock fixes what each reference means.
- Every `cargo` call in an entry point passes `--locked`. A manifest edit with no matching lock update fails the call (measured: `cannot update the lock file … because --locked was passed`), where without the flag Cargo rewrites the lock silently.
- A manifest requirement is the version `cargo add` writes, read from the registry at the time, never recalled. The lock, not the requirement, decides what builds.
- A git dependency is pinned by `rev` to a full commit SHA, never `branch` or `tag`. `deny.toml` sets `[sources] unknown-registry = "deny"` and `unknown-git = "deny"`, so a git source outside `allow-git` fails (measured: `detected 'git' source not explicitly allowed`).
- Advisories are checked by `cargo deny check advisories` against the RustSec database, with `[advisories] yanked = "deny"`. The check fetches the database, so it needs the network.
- The lock moves by `cargo update` in its own change, which carries the audit output.

## Testing

This section states the Rust placement and tooling only.

- **Runner:** `cargo test`, which also runs the doc examples of library targets. Assertions are the standard `assert!`, `assert_eq!` and `assert_ne!`.
- **Unit tests** sit in a `#[cfg(test)] mod tests` at the foot of the file they test, and reach its private items. A test that can be written against the public API goes in `tests/` instead; the in-file module is for a seam that is private.
- **Integration tests** are files in `tests/` at the crate root, each built as its own binary and linking the library target. A subdirectory without a root file builds no test and holds what several files share.
- **E2E tests** are a test-only member at `e2e/` (`publish = false`, no library), listed in `members`. They run the binary the Build entry point produced for the musl target, and none is a unit or integration test of a crate. An integration test of a package that has a binary target can also reach that binary through `env!("CARGO_BIN_EXE_<name>")` (measured), which runs the host build and not the musl one.
- **Substitutes:** a dependency a test replaces is a trait the code under test receives, and the substitute is a hand-written type implementing it. No mock-generation crate is used.
- A test returns `Result<(), E>` and uses `?` where its setup can fail; `unwrap` and `expect` stay allowed there (§Lints).

## Verification commands

These are the commands behind the entry-point names, run from the workspace root.

| Task | Command |
|------|---------|
| Build | `cargo build --workspace --locked` |
| Build (shipped) | `cargo build --release --locked --target <arch>-unknown-linux-musl -p <binary-crate>` |
| Test | `cargo test --workspace --locked` |
| Test (one) | `cargo test --locked -p <crate> <test-name>` |
| Lint | `cargo clippy --workspace --all-targets --locked -- -D warnings` |
| Format check | `cargo fmt --all --check` |
| Format apply | `cargo fmt --all` |
| Audit | `cargo deny --locked check advisories sources` |
| Lock update | `cargo update` |

- **The set is the workspace's members.** Build, Test and Lint pass `--workspace`, which is every member and nothing else. Each prints the member count from `cargo metadata --no-deps --format-version 1` beside the selector, so the count names how the set was chosen. Cargo itself fails on a workspace with no members (measured, rc 101, for build, test and clippy: `The manifest is virtual, and the workspace has no members`).
- **Format's set is the module tree.** `cargo fmt --all` follows `mod` declarations from each member's targets. A `.rs` file no `mod` reaches is neither formatted nor checked (measured: an unreferenced, unformatted file passed `--check`; an unformatted `lib.rs` failed). That set is neither git's nor the directory's.
- Test prints `running N tests` for each target, so a target that ran none is visible in the output; the entry point prints the member count first, as above.
- Build (shipped) is the musl build; plain Build is the host build.
- The entry points unset `RUSTFLAGS` and `CARGO_ENCODED_RUSTFLAGS` (§Lints).
- A task runner wraps these under the entry-point names; the cargo commands stay the same.
