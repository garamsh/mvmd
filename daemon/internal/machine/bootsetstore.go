package machine

import (
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrBootSetAbsent is what a BootSetStore returns for a name it does not hold.
var ErrBootSetAbsent = errors.New("boot set not held by the node")

// ErrBootSetExists is what a BootSetStore's Add returns for a name it holds.
var ErrBootSetExists = errors.New("boot set already held by the node")

// BootFileMismatch is what a BootSetStore's Add returns for a file whose
// contents do not have the digest declared for it.
type BootFileMismatch struct {
	// File is "kernel" or "initrd".
	File string
	Got  string
	Want string
}

func (e *BootFileMismatch) Error() string {
	return fmt.Sprintf("the %s has digest %s, not the declared %s", e.File, e.Got, e.Want)
}

// BootContent is one file of a boot set being added: the digest it must have,
// in hex, and its bytes.
type BootContent struct {
	SHA256  string
	Content io.Reader
}

// BootUpload is the two files of a boot set being added.
type BootUpload struct {
	Kernel BootContent
	Initrd BootContent
}

// BootSetStore holds the boot sets a client added, each as two files only the
// store changes (ADR 0063). A set it returns names files that have the digests
// it returns, until the host's own files are changed under it, which a Start's
// check finds.
type BootSetStore interface {
	// Add reads both files, kernel first, and publishes the set whole under the
	// name or not at all. It returns ErrBootSetExists for a name it holds and
	// *BootFileMismatch for a file that does not have its digest.
	Add(ctx context.Context, name string, upload BootUpload) error
	// Get returns the set under the name, or ErrBootSetAbsent.
	Get(ctx context.Context, name string) (BootSet, error)
	// List returns every set it holds by name.
	List(ctx context.Context) (BootSets, error)
	// Remove deletes the set and its files. An absent name is success.
	Remove(ctx context.Context, name string) error
}
