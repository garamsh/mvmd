.PHONY: lint format format-apply test build generate generate-check ci

# The one statement of the Go toolchain is the toolchain line of go.work.
export GOTOOLCHAIN := $(shell sed -n 's/^toolchain //p' go.work)
# The generator is pinned in its own module, which workspace mode refuses.
PROTOGEN = $(CURDIR)/tooling/protogen/go.mod
TOOL_ENV = GOWORK=off
BUF = $(TOOL_ENV) go tool -modfile=$(PROTOGEN) buf

# The modules every check covers: the ones go.work lists, so a new module
# joins by adding its use line there.
MODULES := $(shell GOTOOLCHAIN=$(GOTOOLCHAIN) go list -m -f '{{.Dir}}')

define each_module
@set -e; \
n=0; \
for dir in $(MODULES); do n=$$((n+1)); $(1); done; \
if [ "$$n" -eq 0 ]; then echo "$@: go.work lists no module" >&2; exit 1; fi; \
echo "$@: covered $$n module(s) selected by go list -m over go.work"
endef

# count_packages prints the packages ./... selects in $$dir and fails on zero.
define count_packages
p=$$(cd "$$dir" && go list ./... | wc -l); \
echo "$@: $$dir: ./... selects $$p package(s)"; \
if [ "$$p" -eq 0 ]; then echo "$@: $$dir: no packages" >&2; exit 1; fi
endef

# The Rust workspaces every check covers, each a directory holding a virtual manifest, and the
# binary crates of them that ship, built for the static musl target.
RUST_WORKSPACES := guest-agent
RUST_BINARIES := guest-agent
RUST_TARGET := $(shell uname -m)-unknown-linux-musl
# A RUSTFLAGS that caps lints would let a denied lint pass, so no cargo call inherits one.
CARGO = env -u RUSTFLAGS -u CARGO_ENCODED_RUSTFLAGS cargo

define each_rust
@set -e; \
n=0; \
for ws in $(RUST_WORKSPACES); do n=$$((n+1)); ( cd "$$ws"; $(1) ); done; \
if [ "$$n" -eq 0 ]; then echo "$@: RUST_WORKSPACES lists no workspace" >&2; exit 1; fi; \
echo "$@: covered $$n Rust workspace(s) listed in RUST_WORKSPACES"
endef

# count_members prints the packages --workspace selects in $$ws and fails on zero.
define count_members
m=$$($(CARGO) metadata --no-deps --format-version 1 | grep -o '"manifest_path"' | wc -l); \
echo "$@: $$ws: --workspace selects $$m member(s) per cargo metadata --no-deps"; \
if [ "$$m" -eq 0 ]; then echo "$@: $$ws: no members" >&2; exit 1; fi
endef

lint:
	$(call each_module, $(count_packages); (cd "$$dir" && go vet ./... && \
	  if [ -f buf.yaml ]; then \
	    lib=$$($(TOOL_ENV) go list -m -f '{{.Version}}' google.golang.org/protobuf); \
	    gen=$$($(TOOL_ENV) go list -modfile=$(PROTOGEN) -m -f '{{.Version}}' google.golang.org/protobuf); \
	    if [ "$$lib" != "$$gen" ]; then echo "$@: $$dir: protobuf $$lib in go.mod but protoc-gen-go $$gen in tooling/protogen" >&2; exit 1; fi; \
	    echo "$@: $$dir: protobuf runtime and protoc-gen-go are both $$lib"; \
	    $(BUF) lint && echo "$@: $$dir: buf lint ok"; \
	  fi))
	$(call each_rust, $(count_members); \
	  $(CARGO) clippy --workspace --all-targets --locked -- -D warnings; \
	  if [ -f buf.yaml ]; then \
	    (cd $(CURDIR)/tooling/protogen && $(BUF) lint "$(CURDIR)/$$ws") && echo "$@: $$ws: buf lint ok"; \
	  fi)

format:
	$(call each_module, \
	files=$$(cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | tr -cd '\0' | wc -c); \
	echo "$@: $$dir: $$files .go file(s) selected by git ls-files --cached --others --exclude-standard"; \
	if [ "$$files" -eq 0 ]; then echo "$@: $$dir: no .go files" >&2; exit 1; fi; \
	bad=$$(cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 gofmt -l); \
	if [ -n "$$bad" ]; then echo "$$bad" >&2; echo "$@: $$dir: unformatted files above" >&2; exit 1; fi)
	$(call each_rust, \
	  files=$$($(CARGO) fmt --all --check -- -v 2>&1 | grep -c '^Formatting ' || true); \
	  echo "$@: $$ws: cargo fmt follows the module tree and checked $$files .rs file(s)"; \
	  if [ "$$files" -eq 0 ]; then echo "$@: $$ws: no .rs files" >&2; exit 1; fi; \
	  $(CARGO) fmt --all --check)

format-apply:
	$(call each_module, (cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 gofmt -w))
	$(call each_rust, $(CARGO) fmt --all)

test:
	$(call each_rust, $(count_members); $(CARGO) test --workspace --locked)
	$(call each_module, $(count_packages); (cd "$$dir" && go test -v ./...))

build:
	$(call each_module, $(count_packages); (cd "$$dir" && go build ./...))
	$(call each_rust, $(count_members); $(CARGO) build --workspace --locked; \
	  for bin in $(RUST_BINARIES); do \
	    $(CARGO) build --release --locked --target $(RUST_TARGET) -p "$$bin"; \
	    echo "$@: $$ws: built $$bin for $(RUST_TARGET)"; \
	  done)

# generate regenerates the committed output of every module that has a buf.gen.yaml.
generate:
	$(call each_module, if [ -f "$$dir/buf.gen.yaml" ]; then (cd "$$dir" && $(BUF) generate) && echo "$@: $$dir: generated"; fi)

# generate-check fails when regenerating changes a file of a module that has a buf.gen.yaml.
generate-check:
	$(call each_module, if [ -f "$$dir/buf.gen.yaml" ]; then \
	  snap() { (cd "$$dir" && git ls-files --cached --others --exclude-standard -z | xargs -0 sha256sum); }; \
	  before=$$(snap); \
	  (cd "$$dir" && $(BUF) generate); \
	  after=$$(snap); \
	  if [ "$$before" != "$$after" ]; then echo "$@: $$dir: regenerating changed files" >&2; exit 1; fi; \
	  echo "$@: $$dir: regenerating changed nothing ($$(printf '%s\n' "$$before" | wc -l) files compared)"; \
	fi)

ci: lint format generate-check test build
