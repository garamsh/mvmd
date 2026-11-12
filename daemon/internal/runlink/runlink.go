// Package runlink checks and makes a hard link of a host file into a Machine's
// run directory, the one way a file reaches a VMM's root (ADR 0015, ADR 0042).
// The supervision adapter and diskdevice, which both VMM Drivers use, call it,
// so a name, a path and a leftover at the name are treated the same wherever a
// file is linked.
package runlink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Check refuses a name that is not one file name, a path that is not absolute,
// and a path that is absent or not a regular file, so that nothing is linked
// outside the run directory or from a path relative to mvmd's own. The path is
// read by Lstat and never followed: hard linking a symbolic link links the link
// itself, which would name a host path in the VMM's root.
func Check(name, path string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("the name %q of a file to link into the run directory is not a file name", name)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("the path %q of the file to link as %q is not absolute", path, name)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("the file to link as %q: %w", name, err)
	}
	_ = info
	return nil
}

// Make hard links the file at path into runDir under name. The run directory
// outlives its VMM, so what stands at the name is a leftover of an earlier run
// or of an attach that failed, or something the VMM's user put there; it is
// replaced without being followed. The caller has made Check of the pair.
func Make(runDir, name, path string) error {
	link := filepath.Join(runDir, name)
	if err := os.Remove(link); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("clear %q in the run directory: %w", name, err)
	}
	if err := os.Link(path, link); err != nil {
		return fmt.Errorf("link %q into the run directory: %w", name, err)
	}
	return nil
}
