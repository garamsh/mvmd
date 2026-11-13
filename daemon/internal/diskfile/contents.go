package diskfile

import (
	"errors"
	"fmt"

	"golang.org/x/sys/unix"
)

// contentsAttr is the extended attribute that holds the SHA-256, in hex, of the
// bytes a disk was made holding, so a repeated create is compared against it
// (ADR 0060). A file without it was made with none.
const contentsAttr = "user.mvmd.contents"

// readContentsDigest returns the digest recorded on the disk file, empty for a
// disk made with no contents.
func readContentsDigest(path string) (string, error) {
	buf := make([]byte, 128)
	n, err := unix.Getxattr(path, contentsAttr, buf)
	switch {
	case errors.Is(err, unix.ENODATA):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the contents digest of %s: %w", path, err)
	}
	return string(buf[:n]), nil
}
