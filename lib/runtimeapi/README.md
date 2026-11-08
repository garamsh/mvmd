# runtimeapi

The mvmd runtime API contract and its Go client, for modules that import it.

- `proto/` holds the authored `.proto` sources; the meaning of each operation is in `docs/architecture/runtime-api.md`.
- `v1alpha1/` holds the generated Go for that version and, beside it, the hand-written outcome and request-validation helpers. A breaking change is a new package beside it.
- `runtimeapi.go` dials the node's Unix socket; the service client comes from the version package.
- `tooling/protogen/` at the repository root pins the generator (`buf` and the Go plugins) apart from `go.mod`. Run `make generate` from the repository root after editing a `.proto` file, and commit the output.
