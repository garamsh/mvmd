package supervision

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// vmmControllers are the controllers a VMM's cgroup limits it with (ADR 0015).
const vmmControllers = "+cpu +memory +pids"

// createVMMCgroup makes the cgroup the Machine's VMM starts in and returns its
// path. A cgroup left by an earlier VMM of the Machine is removed first; it
// cannot hold processes, since no supervisor of the Machine exists.
func (a *Adapter) createVMMCgroup(id machine.MachineID) (string, error) {
	parent := a.cfg.VMMCgroup
	if err := os.MkdirAll(parent, 0o755); err != nil {
		return "", fmt.Errorf("create the VMM cgroup parent: %w", err)
	}
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte(vmmControllers), 0o644); err != nil {
		return "", fmt.Errorf("enable %q in %s: %w", vmmControllers, parent, err)
	}
	path := filepath.Join(parent, string(id))
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return "", fmt.Errorf("remove the VMM cgroup an earlier VMM left: %w", err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return "", fmt.Errorf("create the VMM cgroup: %w", err)
	}
	return path, nil
}

// removeVMMCgroup removes the Machine's VMM cgroup once its processes have
// gone. The kernel releases a cgroup of exited processes a moment after the
// last exit, so a busy cgroup is retried briefly. A cgroup that stays is left
// for the next start of the Machine to remove.
func (a *Adapter) removeVMMCgroup(id machine.MachineID) {
	path := filepath.Join(a.cfg.VMMCgroup, string(id))
	for i := 0; i < 20; i++ {
		err := os.Remove(path)
		if err == nil || errors.Is(err, fs.ErrNotExist) || !errors.Is(err, syscall.EBUSY) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
