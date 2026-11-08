// Package diskfile implements machine.Disk over a directory on the host
// filesystem: sparse raw files formatted ext4 by mkfs.ext4, never mounted.
package diskfile

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ErrBadMKFS is returned when mkfs.ext4 is not available or fails its
// version check.
var ErrBadMKFS = errors.New("mkfs.ext4 not available")

// Disk is a machine.Disk that writes sparse ext4 root disks into a directory.
type Disk struct {
	dir  string
	mkfs string
}

// NewDisk returns a Disk that creates root disks under dir, formatted by the
// mkfs.ext4 binary at mkfs. It verifies mkfs.ext4 -V before returning; a
// failure returns ErrBadMKFS.
func NewDisk(dir, mkfs string) (*Disk, error) {
	if err := checkMKFS(mkfs); err != nil {
		return nil, err
	}
	return &Disk{dir: dir, mkfs: mkfs}, nil
}

// checkMKFS runs mkfs.ext4 -V and returns ErrBadMKFS if it fails.
func checkMKFS(path string) error {
	cmd := exec.Command(path, "-V")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s: %v: %s", ErrBadMKFS, path, err, out)
	}
	return nil
}

// CreateRoot creates a sparse raw file in the directory, formats it ext4 with
// a random UUID, and chowns it to the owner. It returns the disk ID and
// filesystem UUID. On any failure the temporary file is removed.
func (d *Disk) CreateRoot(_ context.Context, owner machine.DiskOwner, sizeBytes uint64) (machine.DiskID, string, error) {
	uuid, err := randomUUID()
	if err != nil {
		return "", "", fmt.Errorf("generate uuid: %w", err)
	}
	name := string(owner.ID) + "-root"
	if !validDiskName(name) {
		return "", "", fmt.Errorf("invalid disk name %q: must not contain / and must not be . or ..", name)
	}
	tmp, err := os.CreateTemp(d.dir, "tmp-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if err := tmp.Truncate(int64(sizeBytes)); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("set size: %w", err)
	}

	if err := runMKFS(d.mkfs, tmpName, uuid); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("format: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("close temp: %w", err)
	}

	if err := os.Chown(tmpName, int(owner.UID), int(owner.GID)); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("chown: %w", err)
	}

	final := filepath.Join(d.dir, name)
	if err := os.Rename(tmpName, final); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("rename: %w", err)
	}

	return machine.DiskID(name), uuid, nil
}

// validDiskName checks that name is safe as a path element: no /, not . or ..
func validDiskName(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	return !bytes.ContainsRune([]byte(name), '/')
}

// validDiskID checks that disk is safe to use in a path under a directory:
// not empty, not . or .., and no /. filepath.Base(id) == id plus the
// empty/dot checks is sufficient; the slash check is explicit for clarity.
func validDiskID(id machine.DiskID) bool {
	s := string(id)
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !bytes.ContainsRune([]byte(s), '/')
}

// randomUUID generates a random v4 UUID, returning an error on read failure.
func randomUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}

// runMKFS runs mkfs.ext4 on the file at path with the given UUID.
func runMKFS(mkfs, path, uuid string) error {
	cmd := exec.Command(mkfs, "-q", "-F", "-U", uuid, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w, output: %s", path, err, string(out))
	}
	return nil
}

// Locate returns the path of a disk the node holds, or ErrDiskAbsent.
// Invalid DiskID is refused and never touches the filesystem.
func (d *Disk) Locate(_ context.Context, disk machine.DiskID) (string, error) {
	if !validDiskID(disk) {
		return "", fmt.Errorf("invalid disk ID %q: must not be empty, ., .., or contain /", disk)
	}
	path := filepath.Join(d.dir, string(disk))
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", machine.ErrDiskAbsent
		}
		return "", err
	}
	return path, nil
}

// Remove deletes the disk file. Invalid DiskID is refused.
// Removing an absent disk succeeds.
func (d *Disk) Remove(_ context.Context, disk machine.DiskID) error {
	if !validDiskID(disk) {
		return fmt.Errorf("invalid disk ID %q: must not be empty, ., .., or contain /", disk)
	}
	path := filepath.Join(d.dir, string(disk))
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("remove disk %s: %w", path, err)
	}
	return nil
}
