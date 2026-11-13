package app

import (
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/pinnedfile"
)

// pinnedBootFiles is the machine.BootFileChecker that reads the file on the
// host and compares its SHA-256 with the one the node pinned (ADR 0058).
type pinnedBootFiles struct{}

// Check implements machine.BootFileChecker.
func (pinnedBootFiles) Check(what string, file machine.KernelFile) error {
	return pinnedfile.Check(what, file.Path, file.SHA256)
}
