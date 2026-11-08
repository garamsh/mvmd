.PHONY: lint format format-apply test build audit generate generate-check ci

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
	$(initrd_twice)

# The Guest Agent's initrd is part of build: it is written to guest-agent/target/initrd.cpio and
# its digest printed. The agent is built a second time in a fresh target directory, and build fails
# unless that build gives the same digest: the initrd is reproducible. The workspace, Cargo's home and the
# target directory are remapped to fixed names, so no path of the machine that builds is in the
# binary, and the digest does not depend on where the checkout is.
define build_initrd
CARGO_TARGET_DIR="$(1)" $(CARGO) build --release --locked --target $(RUST_TARGET) -p guest-agent \
  --config "build.rustflags=[\"--remap-path-prefix=$$workspace=/build\", \"--remap-path-prefix=$$cargo_home=/cargo\", \"--remap-path-prefix=$(1)=/build/target\"]"; \
$(CARGO) run --locked --quiet -p mkinitrd -- "$(1)/$(RUST_TARGET)/release/guest-agent" "$(2)"; \
echo "$@: build $(3): $$(sha256sum "$(2)" | cut -d' ' -f1)  $$(wc -c < "$(2)") bytes"
endef

define initrd_twice
@set -e; \
cd guest-agent; \
workspace=$$PWD; \
cargo_home=$${CARGO_HOME:-$$HOME/.cargo}; \
fresh=$$(mktemp -d); \
trap 'rm -rf "$$fresh"' EXIT; \
$(call build_initrd,$$workspace/target,target/initrd.cpio,1); \
$(call build_initrd,$$fresh/target,$$fresh/initrd.cpio,2); \
if ! cmp -s target/initrd.cpio "$$fresh/initrd.cpio"; then echo "$@: the two initrd builds differ" >&2; exit 1; fi; \
echo "$@: both initrd builds give $$(sha256sum target/initrd.cpio | cut -d' ' -f1)"
endef

# audit checks each Rust workspace's dependencies against the RustSec advisories and its sources.
# It fetches the advisory database, so it needs the network and is not part of ci.
audit:
	$(call each_rust, $(count_members); $(CARGO) deny --locked check advisories sources)

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
