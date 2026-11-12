// Package runlink checks and makes a hard link of a host file into a Machine's
// run directory, the one way a file reaches a VMM's root (ADR 0015, ADR 0042).
// The supervision adapter and both VMM Drivers use it, so a name, a path and a
// leftover at the name are treated the same wherever a file is linked.
package runlink

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Check refuses a name that is not one file name and a path that is not
// absolute, so that nothing is linked outside the run directory or from a path
// relative to mvmd's own. It reads no file.
func Check(name, path string) error {
	if name == "" || name == "." || name == ".." || strings.Contains(name, "/") {
		return fmt.Errorf("the name %q of a file to link into the run directory is not a file name", name)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("the path %q of the file to link as %q is not absolute", path, name)
	}
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
