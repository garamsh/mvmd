package sparsecopy

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// ErrCloneUnsupported is what Clone returns where the file system cannot share
// one file's blocks with another: it has no reflink, or the two files are on
// file systems that cannot share them.
var ErrCloneUnsupported = errors.New("the file system cannot clone a file")

// Cloner makes dst, which must be new and empty, share the blocks of src, or
// returns ErrCloneUnsupported and leaves dst as it was.
type Cloner func(dst, src *os.File) error

// Clone makes dst a reflink of src with FICLONE. It returns
// ErrCloneUnsupported, leaving dst empty, where FICLONE reports EOPNOTSUPP,
// EXDEV, ENOTTY or EINVAL.
func Clone(dst, src *os.File) error {
	err := unix.IoctlFileClone(int(dst.Fd()), int(src.Fd()))
	switch {
	case err == nil:
		return nil
	case errors.Is(err, unix.EOPNOTSUPP), errors.Is(err, unix.EXDEV), errors.Is(err, unix.ENOTTY), errors.Is(err, unix.EINVAL):
		return fmt.Errorf("%w: %v", ErrCloneUnsupported, err)
	}
	return err
}

// Duplicate makes the new, empty dst a copy of src: a reflink where supported,
// else Copy. Any other clone failure is returned, not copied over.
func Duplicate(dst, src *os.File) (cloned bool, err error) {
	return DuplicateWith(Clone, dst, src)
}

// DuplicateWith is Duplicate with the clone it tries given, which a test
// replaces to stand for a file system that has reflinks and one that has not.
func DuplicateWith(clone Cloner, dst, src *os.File) (cloned bool, err error) {
	switch err := clone(dst, src); {
	case err == nil:
		return true, nil
	case errors.Is(err, ErrCloneUnsupported):
		return false, Copy(dst, src)
	default:
		return false, err
	}
}
