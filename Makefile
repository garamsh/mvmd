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

lint:
	$(call each_module, $(count_packages); (cd "$$dir" && go vet ./... && \
	  if [ -f buf.yaml ]; then \
	    lib=$$($(TOOL_ENV) go list -m -f '{{.Version}}' google.golang.org/protobuf); \
	    gen=$$($(TOOL_ENV) go list -modfile=$(PROTOGEN) -m -f '{{.Version}}' google.golang.org/protobuf); \
	    if [ "$$lib" != "$$gen" ]; then echo "$@: $$dir: protobuf $$lib in go.mod but protoc-gen-go $$gen in tooling/protogen" >&2; exit 1; fi; \
	    echo "$@: $$dir: protobuf runtime and protoc-gen-go are both $$lib"; \
	    $(BUF) lint && echo "$@: $$dir: buf lint ok"; \
	  fi))

format:
	$(call each_module, \
	files=$$(cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | tr -cd '\0' | wc -c); \
	echo "$@: $$dir: $$files .go file(s) selected by git ls-files --cached --others --exclude-standard"; \
	if [ "$$files" -eq 0 ]; then echo "$@: $$dir: no .go files" >&2; exit 1; fi; \
	bad=$$(cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 gofmt -l); \
	if [ -n "$$bad" ]; then echo "$$bad" >&2; echo "$@: $$dir: unformatted files above" >&2; exit 1; fi)

format-apply:
	$(call each_module, (cd "$$dir" && git ls-files --cached --others --exclude-standard -z -- '*.go' | xargs -0 gofmt -w))

test:
	$(call each_module, $(count_packages); (cd "$$dir" && go test -v ./...))

build:
	$(call each_module, $(count_packages); (cd "$$dir" && go build ./...))

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
