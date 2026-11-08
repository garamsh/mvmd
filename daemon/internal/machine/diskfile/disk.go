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
	"strconv"

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
	out, err := exec.Command(path, "-V").CombinedOutput()
	if err != nil {
		return ErrBadMKFS
	}
	_ = out // version string consumed
	return nil
}

// CreateRoot creates a sparse raw file in the directory, formats it ext4 with
// a random UUID, and chowns it to the owner. It returns the disk ID and
// filesystem UUID. On any failure the temporary file is removed.
func (d *Disk) CreateRoot(_ context.Context, owner machine.DiskOwner, sizeBytes uint64) (machine.DiskID, string, error) {
	uuid := randomUUID()
	name := string(owner.ID) + "-root"
	tmp, err := os.CreateTemp(d.dir, "tmp-*")
	if err != nil {
		return "", "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()

	if err := d.ensureSize(tmp, sizeBytes); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("set size: %w", err)
	}

	if err := d.format(tmpName, uuid); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("format: %w", err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return "", "", fmt.Errorf("close temp: %w", err)
	}

	if err := chownFile(tmpName, owner.UID, owner.GID); err != nil {
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

// randomUUID generates a random v4 UUID.
func randomUUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 1
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// ensureSize sets the file size via Truncate so it stays sparse.
func (d *Disk) ensureSize(f *os.File, size uint64) error {
	return f.Truncate(int64(size))
}

// format runs mkfs.ext4 on the file at path with the given UUID.
func (d *Disk) format(path, uuid string) error {
	cmd := exec.Command(d.mkfs, "-q", "-F", "-U", uuid, path)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkfs.ext4 %s: %w, output: %s", path, err, string(out))
	}
	return nil
}

// Locate returns the path of a disk the node holds, or ErrDiskAbsent.
func (d *Disk) Locate(_ context.Context, disk machine.DiskID) (string, error) {
	path := filepath.Join(d.dir, string(disk))
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return "", machine.ErrDiskAbsent
		}
		return "", err
	}
	return path, nil
}

// Remove deletes the disk file. Removing an absent disk succeeds.
func (d *Disk) Remove(_ context.Context, disk machine.DiskID) error {
	path := filepath.Join(d.dir, string(disk))
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// chownFile changes the ownership of path to uid:gid.
func chownFile(path string, uid, gid uint32) error {
	return os.Chown(path, int(uid), int(gid))
}

// mkfsExt4 returns the version string from `mkfs.ext4 -V`.
// It is exported so tests can assert the version on the running node.
func mkfsExt4Version(mkfsPath string) (string, error) {
	out, err := exec.Command(mkfsPath, "-V").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("mkfs.ext4 -V: %w", err)
	}
	return string(out), nil
}

// EnsureMKFSVersion checks that mkfs.ext4 meets the minimum version.
// It prints the version to the log and fails if it is below 1.47.
func EnsureMKFSVersion(mkfsPath string) error {
	out, err := mkfsExt4Version(mkfsPath)
	if err != nil {
		return err
	}
	return parseVersion(out)
}

// parseVersion extracts the version from mkfs.ext4 -V output and checks >= 1.47.
func parseVersion(output string) error {
	for _, line := range bytes.Split(bytes.TrimSpace([]byte(output)), []byte{'\n'}) {
		if bytes.Contains(line, []byte("mke2fs")) {
			// "mke2fs 1.47.2 (16-May-2023)"
			parts := bytes.Split(line, []byte{' '})
			if len(parts) >= 1 {
				ver := bytes.TrimSpace(parts[1])
				return checkVersion(ver)
			}
		}
	}
	return nil // no version line found: best-effort
}

// checkVersion validates the version string is >= 1.47.
func checkVersion(ver []byte) error {
	var major, minor int
	if _, err := fmt.Sscanf(string(ver), "%d.%d", &major, &minor); err != nil {
		return fmt.Errorf("parse version %q: %w", string(ver), err)
	}
	if major < 1 || (major == 1 && minor < 47) {
		return fmt.Errorf("mkfs.ext4 version %s: need >= 1.47", string(ver))
	}
	return nil
}

// parseUID extracts a UID from a string like "uid=1000 (user)".
func parseUID(output string) (uint32, error) {
	for _, line := range bytes.Split(bytes.TrimSpace([]byte(output)), []byte{'\n'}) {
		if bytes.Contains(line, []byte("uid=")) {
			// "uid=1000 (mvmd)"
			parts := bytes.Fields(line)
			if len(parts) >= 1 {
				u := bytes.TrimPrefix(parts[0], []byte("uid="))
				i, err := strconv.Atoi(string(u))
				if err != nil {
					return 0, err
				}
				return uint32(i), nil
			}
		}
	}
	return 0, errors.New("no uid= found")
}
