# Go — Architecture & Style Conventions

> How a Go module is laid out and where each kind of code belongs:
> directories, naming, errors, logging, comments, tests, imports.
>
> Checked against go1.21.13, mockery v3.7.4, goimports
> (golang.org/x/tools) v0.49.0 and golangci-lint v2.13.2. A claim below
> that names no version holds for these.

## Contents
- 0. Folder & file naming — strict
- 1. Directory layout
- 2. Module / package boundary
- 3. Naming
- 4. Error handling
- 5. Logging & observability
- 6. Comments & docs
- 7. Testing
- 8. Imports & dependencies
- 9. Verification commands

## 0. Folder & file naming — strict

**Banned at any level:** `model.go`.

Files named for a concept: `password_hashing.go`, `format_currency.go`.

## 1. Directory layout

See §0 for the banned-name list.

**What the module is decides which layout governs it**, not how large
it is:

- **Service layout** — the module ships a binary.
- **Library layout** — the module ships no binary and exists for other
  modules to import.

A module keeps that layout at any size, and changes layout only by
gaining or losing `cmd/<binary>/main.go` — a change to the tree, not a
line count crossed unnoticed.

### Service layout

- `cmd/<binary>/main.go` — the binary's entry point.
- `internal/<domain>/<domain>.go` — the aggregate: domain types, DTOs
  and sentinel errors (`User`, `CreateUserInput`, `ErrUserNotFound`).
- `internal/<domain>/service.go` — what the domain offers: `type
  Service interface { ... }`, unexported `type service struct { ... }`,
  `NewService(...) Service`. A domain owes one once it has behaviour of
  its own to offer. Holding types and persisting them is not that, and
  neither is translating a request, composing commands or wiring a
  runtime.
- `internal/<domain>/<name>.go` — one dependency interface the domain
  needs, named for it: `repository.go` declares `type Repository
  interface`, `mailer.go` declares `type Mailer interface`. No
  condition and no threshold — the side that needs the behaviour
  declares the contract, and one file holds one contract.
- `internal/<domain>/<group>.go` — the service's method bodies
  (`create.go`, `update.go`, `lifecycle.go`).
- `internal/<domain>/service_test.go` — the service's tests (§7).
- `internal/<domain>/<pkg>/` — the implementations of that dependency
  interface (`repository/postgres.go` or `memory/repository.go`); the
  in-memory one serves tests and dev.
- `internal/<crosscutting>/` — a concern extracted below the domains,
  named by what it is.
- `go.mod`, `go.sum` — module root.

**Rules for `internal/<domain>/`:**

- Split the method bodies out of `service.go` only when the service
  has more than one verb; a single-method service keeps that method
  in `service.go`. One file per verb, named for it (`create.go`,
  `update.go`); where several verbs serve one responsibility they
  share one file, named for the responsibility (`lifecycle.go` for
  create/activate/deactivate).
- **An implementation that imports a driver (`pgx`, a vendor SDK)
  sits outside the domain package's import graph**, so the driver
  stays out of that package and its tests; where unexported fields
  close the aggregate, only its own package can construct one, so the
  implementation stays beside it and this rule does not reach that
  domain.

**One file vs several** inside `internal/<domain>/<pkg>/`:

- **One file per implementation** (`postgres.go`, `memory.go`) when it
  is ≤ ~300 LoC and has no private helpers worth isolating.
- **Several files for one implementation** (`postgres.go`,
  `postgres_queries.go`) when it is > ~300 LoC or owns private
  helpers / connection-pool / per-SQL constants.

### Library layout

- `<pkg>.go`, `<pkg>_test.go`, `go.mod`, `README.md` — all at the
  module root. `command.go`, `args.go` — one file per thing it owns.

### Root-level files

A root-level file exists when the module has that concern, and it is
named after the concern:

- `errors.go` — sentinels no single domain owns (§4).
- `config.go` — the module's configuration type and its loading.
- `logger.go` — only when logger setup goes past `slog.Default()`
  (§5). A project that calls `slog.Default()` directly has no
  root-level logger file.
- `httpserver.go` — server construction and route wiring, when the
  module serves HTTP.

Multiple root-level files are fine: several concerns (`errors.go` +
`logger.go` + `config.go`), or one concern across several files
(`httpserver.go` + `httpserver_routes.go`), each named after what it
holds. The root is one package, so a second file for a concern is a
name and not a boundary.

### Project envelope

This section states which Go file holds the composition site, and
what `internal/` and `pkg/` hold.

- **The composition site is `cmd/<binary>/main.go`, unless a test outside `cmd/<binary>/` has to reach it** — a suite in `tests/` (§7) among them. Only a test in its own directory can import a main package, so where such a test exercises the wired binary the site is in a package `main.go` calls, and `main.go` holds that call. A site a test cannot call is one the test copies instead, and the copy goes on passing after the shipped wiring breaks.
- `internal/` is enforced by the Go toolchain. Use it for everything not explicitly public.
- `pkg/` is for code other modules import. Most services don't need it.

## 2. Module / package boundary

§1 owns the per-domain file inventory. This section states which Go
file declares and which depends.

- `internal/<domain>/<domain>.go`, `internal/<domain>/service.go` and
  `internal/<domain>/<name>.go` declare the types, sentinels, `Service`
  interface and dependency interfaces the rest of the domain is written
  against.
- `internal/<domain>/<group>.go` and the implementations in
  `internal/<domain>/<pkg>/` depend on those declarations.
- `mocks/` — generated by mockery (§7) from the interfaces those files
  declare.
- **The import goes one way.** `internal/<domain>/<pkg>/` may import
  `internal/<domain>` for the types in the interface's signatures; the
  reverse import never happens.
- Go rejects an import cycle at compile time, and a struct holding
  itself by value through another (`type A struct{ b B }`, `type B
  struct{ a A }`) as `invalid recursive type`. A cycle through a
  pointer, slice, map or interface compiles; those are found by
  reading.

## 3. Naming

This section states only the form each kind of Go identifier takes.

- **Packages:** single word, lowercase, no underscores. Singular for
  one kind of thing (`user`, `order`); pluralize only for genuine
  collections (`errors`, `flags`).
- **Types:** `MixedCaps`, no underscores (`UserService`).
- **Interfaces:** the behaviour required, in the agent form of its verb
  (`Storer`, `Mailer`, `Validator`). A dependency that is a thing rather
  than an action keeps the thing's name — `Repository`, `Clock`,
  `Session`; a `Model` does not become a `Modeller`. Where that name is
  also one of the domain's verbs, the agent form breaks the tie: a
  `Store` verb, a `Storer` interface, one file each (§1).
- **Functions / methods:** `MixedCaps`, verb-noun for one that does
  work (`GetUser`, `ParseToken`). A method that only returns a field
  is named after the field: `Name()`, never `GetName()`.
- **Constants:** `MixedCaps` (not `MAX_SIZE`). Group in `const ( ... )`.
- **Variables:** short in small scopes, `MixedCaps` for package-level.
- **Acronyms:** all-caps for the common form, consistent case
  (`HTTPClient`, `URLParser`, `ID`).
- **Receivers:** short, consistent across methods of the same type
  (`s *Service`).
- **Initialisms:** `URL`, `ID`, `HTTP`, `JSON`, `XML`, `API`, `SQL` —
  always uppercase or lowercase, never mixed.

## 4. Error handling

This section states the Go error forms, and which file or layer
translates, wraps, logs or recovers one.

- **An adapter translates before it returns.**
  `repository/postgres.go` turns `pgx.ErrNoRows` into
  `ErrUserNotFound`; the driver's error does not leave the file that
  imports the driver. The errors an implementation returns are part of
  the interface it satisfies, so one that leaks its library's errors is
  not one `repository/memory.go` can stand in for.
- **Sentinel** when the caller branches on which failure and the
  failure carries no data: `var ErrNotFound = errors.New("not
  found")`, read with `errors.Is`.
- **Typed** when the caller needs data out of the failure: a struct
  with `Error()`, read with `errors.As`, and `Is(target error) bool`
  or `Unwrap() error` where it wraps another.
- **Opaque** when the caller has no business branching. A sentinel or
  a type a caller can match on is API you have to keep working; where
  nothing needs to branch, publish neither.
- **`%w` publishes the error it wraps.** A caller can reach through
  it with `errors.Is` and `errors.As`, so replacing what is inside
  breaks them later. Wrap with `%w` where a caller is meant to branch
  on the wrapped error, `%v` where it is not, `errors.Join` where the
  caller needs all of several.
- Add context at a boundary — network, IO, an external call — not on
  every line: `fmt.Errorf("create user: %w", err)`.
- **Each transport translates in one place.** Whatever implements a
  transport owns the mapping from domain error to that transport's
  codes — `httpserver.go` answering 404 to `ErrUserNotFound`, or the
  type satisfying a generated gRPC service — and nothing else names
  one. A second transport gets its own translator, not a share of the
  first. A domain outlives the transport it is served over, and a
  mapping spread across handlers gives one sentinel several codes.
- **Don't log and return.** Return, and let the outermost boundary log
  once: the transport translator where the module has one, `main`
  where it has none. Logging on the way up prints one failure several
  times, some lines carrying the request's `trace_id` and some not.
- **A panic does not cross a package boundary.** A violated invariant
  may panic — recovering from one hides the bug that caused it — but
  that ends the program rather than answering the caller. An HTTP
  server still holds a recover middleware: `net/http` recovers a
  handler's panic and logs the stack itself, but aborts the response
  instead of answering — the client gets a closed connection, or an
  HTTP/2 `RST_STREAM` — so the middleware exists to reply 500.

## 5. Logging & observability

- Stdlib `log/slog` (Go 1.21+). Prefer it over `log` and third-party
  loggers for new code.
- Levels: `Debug`, `Info`, `Warn`, `Error`.
- Required fields for HTTP / RPC handlers: `trace_id` (from
  OpenTelemetry's request context — falls back to a
  locally-generated UUID if no span exists) and `user_id`
  / `account_id` when the principal is known. The older
  `request_id` field still appears in many log pipelines but is
  really a stand-in for `trace_id`; new code emits the latter.
- Emit the operation name and key parameters as well.
- **Never** log secrets, passwords, tokens, full request bodies that
  may contain PII.
- Metrics: `prometheus/client_golang`. Define the registry once.
- Tracing: OpenTelemetry (`go.opentelemetry.io/otel`).

## 6. Comments & docs

This section states the Go comment form only.

- Doc comments begin with the name being declared.
- Package comment in `doc.go` (one short sentence — e.g.
  `// Package auth provides ...`) is conventional and surfaces on
  pkg.go.dev. Long descriptions belong in `service.go` as a doc
  comment on the service struct.

## 7. Testing

This section states the Go test placement, tooling and test-name
form.

**Runner:** stdlib `testing`, files end with `_test.go`.
**Assertions:** `testify/assert` + `testify/require` (default unless
project says otherwise).

**Organization:**

- **External tests** (`package user_test`): next to source. Reach the
  unit under test through its exported API. **Default to this.**
- **Internal tests** (`package user`): same directory and package as
  the code under test. Use only when you genuinely need a white-box
  seam (uncommon).
- **`tests/` at module root:** integration / E2E tests that wire
  multiple domains. Separate binary.
- **In-process integration test client:** `httptest.NewServer`.
- **E2E tooling:** the binary under test is what `go build` produces;
  real services come from containers
  (`github.com/testcontainers/testcontainers-go`).

Within `internal/<domain>/`:

- Tests for `<group>.go` live in `<group>_test.go`.
- Table-driven subtests: `tests := []struct{ name string; ... }{...}`,
  run through `t.Run("case name", ...)`.
- Test names: `TestFunctionName` or `TestFunctionName_Scenario`.
- Benchmarks: `func BenchmarkXxx(b *testing.B)`.

### Generated mocks — tool, configuration, output, injection

Where the project generates mocks, generate them with
**[mockery v3](https://vektra.github.io/mockery/)**.

- **Config:** `.mockery.yml` at the module root — or `.mockery.yaml`,
  which mockery reads equally. One or the other, not both. It declares
  which interfaces to mock, output directory, package names,
  per-interface overrides.
- **Generated location:** declared in that file by `dir`, the output
  directory, with `filename` for the file itself — at the top level,
  or per interface in that interface's `config:` block under
  `interfaces:`. With neither set, mockery writes `mocks_test.go` in
  the interface's own directory and package, so
  `mocks/<package>/<Interface>.go` at module root is one `dir` and
  `filename` declare. A dependency interface is scoped to the one
  domain that declares it, so `dir: internal/<domain>/mocks/` fits it
  too. A project whose tests live outside the interface's directory
  — a suite in `tests/` among them — sets `dir` and `filename` to a
  location those tests can import, because a `_test.go` mock is not
  importable across directories. Pick one convention per project.
- **Generation:** `mockery` (reads config) or `go generate ./...`
  when interfaces carry `//go:generate mockery` directives. Pick one.
- **In tests:** the generated mock satisfies the interface; pass it
  as a constructor argument (`NewService(repo, mailer, logger)`).

## 8. Imports & dependencies

Three groups, separated by blank lines:

1. Standard library
2. Third-party
3. Internal module

`goimports -local <module path>` produces the third group. Plain
`goimports` emits two — standard library and everything else — so the
`-local` flag is what separates the internal module.
Use `go mod tidy` after every change. Don't commit `go.sum` updates
you don't recognize. `go vet ./...`. `golangci-lint run` if
configured.

## 9. Verification commands

These are the commands that sit behind the project's entry-point
names.

`go test` prints only `ok <pkg>` for a package that passes and discards
the rest of its output; the full output appears on failure, or under
`-v`. So anything a test binary prints about what it covered is missing
from exactly the run that needed it. An entry point that has something
to report prints it itself, before invoking `go test`.

| Task | Command |
|------|---------|
| Build | `go build ./...` |
| Test | `go test ./...` |
| Test (one) | `go test -run TestName ./pkg/...` |
| Vet | `go vet ./...` |
| Format check | `gofmt -l` over `git ls-files --cached --others --exclude-standard -z -- '*.go'` |
| Format apply | `gofmt -w` over the same set |
| Module tidy | `go mod tidy` |
| Lint (if configured) | `golangci-lint run` |
| Coverage | `go test -cover ./...` |
| Generate (mockery) | `mockery` (config) or `go generate ./...` |

The format rows take their set from git: CI checks out the tracked
set, a `.` walk also reaches ignored trees CI never sees and `-w`
rewrites them, `--cached --others --exclude-standard` adds back the
untracked files the ignore files leave so that a new file fails here
before it fails in CI, and the set reaches `gofmt` as arguments rather
than through a shell substitution, because `gofmt` with no file
arguments reads standard input: over an empty set `gofmt -l $(…)`
blocks, where `xargs -0` hands the process `/dev/null` and it returns.

The Build, Test, Vet and Coverage rows take their set from the working
tree: `./...` is the module's packages as the directory on disk holds
them, which is neither git's set nor CI's checkout. It never consults
git. It skips a directory named `testdata` or beginning with `.` or
`_`, tracked or not, by the go tool's own rule, and reaches an ignored
directory with any other name as it reaches a tracked one. So a package
in such a directory is built here and never in CI: a failure there is
not a CI failure, and a pass here is not a CI pass.

The Build, Test, Vet and Coverage entry points each run `go list ./...`
before their command, print the number of packages it lists beside the
selector `./...`, and fail where that number is zero. `go list ./...`
selects as the command after it does, so the number counts the
packages `./...` selected, not what the command built, tested or
vetted. `go list` exits 0 when it lists nothing, so the entry point
compares the number itself. Over a module holding no packages, `go
build ./...` exits 0 and prints only `go: warning: "./..." matched no
packages`; `go test ./...`, `go test -cover ./...` and `go vet ./...`
exit 1 there, with `no packages to test` or `no packages to vet`.

Every tool the module invokes is built with a toolchain at least the
module's `go` directive. `go install tool@version` and `go run
tool@version` take the toolchain from the tool's own `go.mod`, not from
this module — mockery 3.7.3 declares `go 1.25.5` — so a tool whose
directive is older yields a binary that refuses to load the module's
packages, fatally and at package-load time. Set that floor once for the
module rather than per tool: `GOTOOLCHAIN` with an `+auto` suffix sets a
minimum without stopping a tool that needs more.

A task runner (Taskfile, Mage, Make) wraps these under the entry-point
names; the underlying go commands stay the same.

