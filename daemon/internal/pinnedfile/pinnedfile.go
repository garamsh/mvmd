// Package pinnedfile checks that a file is the one a node pinned, by the
// SHA-256 of its contents.
package pinnedfile

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
)

// digest returns the SHA-256 of the file, in hex.
func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Check refuses a file whose digest is not the pinned one, or that cannot be
// read. what names the file in the error, as in "the firecracker kernel".
func Check(what, path, want string) error {
	got, err := digest(path)
	if err != nil {
		return fmt.Errorf("check %s: %w", what, err)
	}
	if got != want {
		return fmt.Errorf("%s %s has digest %s, not the configured %s", what, path, got, want)
	}
	return nil
}
