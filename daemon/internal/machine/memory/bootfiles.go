package memory

import (
	"fmt"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// BootFiles is an in-memory machine.BootFileChecker: every file has its pinned
// digest until Change says it has not.
type BootFiles struct {
	mu      sync.Mutex
	changed map[string]bool
}

// Change makes the file at path fail its check from now on.
func (b *BootFiles) Change(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed == nil {
		b.changed = make(map[string]bool)
	}
	b.changed[path] = true
}

// Check implements machine.BootFileChecker.
func (b *BootFiles) Check(what string, file machine.KernelFile) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.changed[file.Path] {
		return fmt.Errorf("%s %s has changed from the digest %s", what, file.Path, file.SHA256)
	}
	return nil
}
