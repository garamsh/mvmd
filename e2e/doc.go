// Package e2e holds the end-to-end tests of mvmd: the built binary, run against real
// infrastructure, with nothing mocked (docs/convention/testing.md). The tests are in files behind
// the firecracker_real build tag, which `make test-e2e` sets; this file is what lets
// `go list ./...` see the package without the tag, so the checks that count packages count it.
package e2e
