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
# binary crates of them that ship, as workspace:crate, built for the static musl target.
RUST_WORKSPACES := guest-agent imageconv
RUST_BINARIES := guest-agent:guest-agent imageconv:imageconv
RUST_TARGET := $(shell uname -m)-unknown-linux-musl
# The pinned erofs-utils tools, built by imageconv/mkfs-erofs/build.sh. mkfs.erofs goes into the
# converter's initrd; the tests read what it writes with fsck.erofs.
EROFS_UTILS := imageconv/target/erofs-utils
EROFS_UTILS_BUILD := imageconv/target/erofs-utils-build
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

# The tests of test-firecracker and test-cloud-hypervisor are behind build tags, so vet reads them again with them.
lint:
	$(call each_module, $(count_packages); (cd "$$dir" && go vet ./... && go vet -tags firecracker_real,cloud_hypervisor_real ./... && \
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

# erofs-utils builds mkfs.erofs and fsck.erofs, statically linked, from pinned sources. The script
# builds each twice and fails unless the two are identical; it skips the work when its pins are
# unchanged since the last build, and `rebuild` forces it.
.PHONY: erofs-utils rebuild-erofs-utils
erofs-utils:
	@imageconv/mkfs-erofs/build.sh $(EROFS_UTILS_BUILD) $(EROFS_UTILS)

rebuild-erofs-utils:
	@imageconv/mkfs-erofs/build.sh $(EROFS_UTILS_BUILD) $(EROFS_UTILS) --rebuild

# The converter's tests convert with the pinned mkfs.erofs and read the result with fsck.erofs.
test: export MKFS_EROFS := $(CURDIR)/$(EROFS_UTILS)/mkfs.erofs
test: export FSCK_EROFS := $(CURDIR)/$(EROFS_UTILS)/fsck.erofs
# The daemon's channel tests run the host-side Guest Agent binary, which the line below builds for
# the host and names to them.
test: export GUEST_AGENT_CHANNEL := $(CURDIR)/guest-agent/target/debug/guest-agent-channel
# The daemon's Converter channel tests run the host-side converter binary, built and named the same
# way.
test: export CONVERTER_CHANNEL := $(CURDIR)/imageconv/target/debug/imageconv-channel
test: erofs-utils
	$(call each_rust, $(count_members); $(CARGO) test --workspace --locked)
	cd guest-agent && $(CARGO) build --locked -p guest-agent --bin guest-agent-channel
	cd imageconv && $(CARGO) build --locked -p imageconv --bin imageconv-channel
	$(call each_module, $(count_packages); (cd "$$dir" && go test -v ./...))

# test-firecracker boots the pinned Firecracker on the pinned kernel with the Guest Agent's initrd,
# through the Driver, as the user running it. It needs read and write access to /dev/kvm, and it is
# not part of ci: a runner without KVM would fail it, so the pipeline runs it as a job of its own.
# tooling/firecracker/fetch.sh fetches the two inputs by their pinned digests; a missing input fails
# the tests and never skips them.
.PHONY: guest-initrd test-firecracker test-cloud-hypervisor
guest-initrd:
	$(call initrd_twice,guest-agent,)

test-firecracker: guest-initrd
	@set -e; \
	tooling/firecracker/fetch.sh tooling/firecracker/target; \
	set -a; . tooling/firecracker/target/inputs.env; set +a; \
	export MVMD_INITRD=$(CURDIR)/guest-agent/target/initrd.cpio; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd daemon && go test -tags firecracker_real -count=1 -v -run '^TestReal_' ./internal/firecracker/) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -c '^--- PASS: TestReal_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -run '^TestReal_' under the firecracker_real tag"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

# test-cloud-hypervisor does the same for the pinned Cloud Hypervisor, through its Driver. The kernel
# is the one tooling/firecracker/fetch.sh pins, so that script runs for it as well; the two inputs
# files are sourced together. Like test-firecracker it is not part of ci and a missing input fails.
test-cloud-hypervisor: guest-initrd
	@set -e; \
	tooling/firecracker/fetch.sh tooling/firecracker/target; \
	tooling/cloud-hypervisor/fetch.sh tooling/cloud-hypervisor/target; \
	set -a; . tooling/firecracker/target/inputs.env; . tooling/cloud-hypervisor/target/inputs.env; set +a; \
	export MVMD_INITRD=$(CURDIR)/guest-agent/target/initrd.cpio; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd daemon && go test -tags cloud_hypervisor_real -count=1 -v -run '^TestReal_' ./internal/cloudhypervisor/) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -c '^--- PASS: TestReal_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -run '^TestReal_' under the cloud_hypervisor_real tag"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

# test-e2e runs the end-to-end tests of e2e/: the built mvmd and mvmd-supervisor on the pinned
# Firecracker and kernel, with the Guest Agent's and the converter's initrds, against a registry the
# tests serve themselves. The tests pull images through the runtime API, read what the converter
# microVM made, and check the pre-boot check of the cache. They run as root, and under passwordless
# sudo when this user is not; they need read and write access to /dev/kvm and a Linux with
# fs-verity on ext4 and loop mounts. Like test-firecracker, it is not part of ci and a missing
# input fails the tests and never skips them.
.PHONY: converter-initrd test-e2e
converter-initrd: erofs-utils
	$(call initrd_twice,imageconv,"$$workspace/target/erofs-utils/mkfs.erofs")

test-e2e: guest-initrd converter-initrd
	@set -e; \
	mkdir -p e2e/target; \
	(cd daemon && go build -o $(CURDIR)/e2e/target/mvmd ./cmd/mvmd && go build -o $(CURDIR)/e2e/target/mvmd-supervisor ./cmd/mvmd-supervisor); \
	tooling/firecracker/fetch.sh tooling/firecracker/target; \
	set -a; . tooling/firecracker/target/inputs.env; set +a; \
	export MVMD_INITRD=$(CURDIR)/guest-agent/target/initrd.cpio; \
	export MVMD_E2E_CONVERTER_INITRD=$(CURDIR)/imageconv/target/initrd.cpio; \
	export MVMD_E2E_MVMD=$(CURDIR)/e2e/target/mvmd; \
	export MVMD_E2E_SUPERVISOR=$(CURDIR)/e2e/target/mvmd-supervisor; \
	export FSCK_EROFS=$(CURDIR)/$(EROFS_UTILS)/fsck.erofs; \
	log=$$(mktemp); trap 'rm -f "$$log"' EXIT; \
	if ! (cd e2e && go test -tags firecracker_real -count=1 -v -run '^TestE2E_' ./...) >"$$log" 2>&1; then cat "$$log"; exit 1; fi; \
	cat "$$log"; \
	n=$$(grep -c '^--- PASS: TestE2E_' "$$log" || true); \
	echo "$@: $$n test(s) passed, selected by -run '^TestE2E_' under the firecracker_real tag"; \
	if [ "$$n" -eq 0 ]; then echo "$@: no test ran" >&2; exit 1; fi

build: rebuild-erofs-utils
	$(call each_module, $(count_packages); (cd "$$dir" && go build ./...))
	$(call each_rust, $(count_members); $(CARGO) build --workspace --locked; \
	  for pair in $(RUST_BINARIES); do \
	    if [ "$${pair%%:*}" = "$$ws" ]; then \
	      bin="$${pair#*:}"; \
	      $(CARGO) build --release --locked --target $(RUST_TARGET) -p "$$bin"; \
	      echo "$@: $$ws: built $$bin for $(RUST_TARGET)"; \
	    fi; \
	  done)
	$(call initrd_twice,guest-agent,)
	$(call initrd_twice,imageconv,"$$workspace/target/erofs-utils/mkfs.erofs")

# Each initrd is part of build: it is written to <workspace>/target/initrd.cpio and its digest
# printed. The binary is built a second time in a fresh target directory, and build fails unless
# that build gives the same digest: the initrd is reproducible. The workspace, Cargo's home and the
# target directory are remapped to fixed names, so no path of the machine that builds is in the
# binary, and the digest does not depend on where the checkout is. The converter's initrd also
# holds the pinned mkfs.erofs, which build.sh has built twice and compared.
# build_initrd takes the target directory, the output, the build's number, the crate and the
# files the initrd holds beside the crate's binary.
define build_initrd
CARGO_TARGET_DIR="$(1)" $(CARGO) build --release --locked --target $(RUST_TARGET) -p $(4) \
  --config "build.rustflags=[\"--remap-path-prefix=$$workspace=/build\", \"--remap-path-prefix=$$cargo_home=/cargo\", \"--remap-path-prefix=$(1)=/build/target\"]"; \
$(CARGO) run --locked --quiet -p mkinitrd -- "$(1)/$(RUST_TARGET)/release/$(4)" $(5) "$(2)"; \
echo "$@: $(4) build $(3): $$(sha256sum "$(2)" | cut -d' ' -f1)  $$(wc -c < "$(2)") bytes"
endef

# initrd_twice takes the crate, which names its workspace directory, and the files its initrd
# holds beside its binary.
define initrd_twice
@set -e; \
cd $(1); \
workspace=$$PWD; \
cargo_home=$${CARGO_HOME:-$$HOME/.cargo}; \
fresh=$$(mktemp -d); \
trap 'rm -rf "$$fresh"' EXIT; \
$(call build_initrd,$$workspace/target,target/initrd.cpio,1,$(1),$(2)); \
$(call build_initrd,$$fresh/target,$$fresh/initrd.cpio,2,$(1),$(2)); \
if ! cmp -s target/initrd.cpio "$$fresh/initrd.cpio"; then echo "$@: the two $(1) initrd builds differ" >&2; exit 1; fi; \
echo "$@: both $(1) initrd builds give $$(sha256sum target/initrd.cpio | cut -d' ' -f1)"
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
