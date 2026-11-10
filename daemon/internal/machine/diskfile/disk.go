// Package diskfile implements machine.Disk over a directory on the host
// filesystem: sparse raw files formatted ext4 by mkfs.ext4, never mounted.
package diskfile

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

var _ machine.Disk = (*Disk)(nil)

// ErrBadMKFS is returned when mkfs.ext4 is not available or fails its
// version check.
var ErrBadMKFS = errors.New("mkfs.ext4 not available")

// tempPrefix starts the name of a file a disk is built in before it is
// published. No disk has such a name, so no unfinished file is listed.
const tempPrefix = "tmp-"

// requestAttr is the extended attribute that holds the request ID of the call
// that made a disk, so the disk and its record are published together.
const requestAttr = "user.mvmd.request"

// Disk is a machine.Disk that writes sparse ext4 disks into a directory.
type Disk struct {
	dir  string
	mkfs string
}

// NewDisk returns a Disk that creates disks under dir, formatted by the
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
	name := string(machine.RootDiskID(owner.ID))
	if !validDiskName(name) {
		return "", "", fmt.Errorf("invalid disk name %q: must not contain / and must not be . or ..", name)
	}
	tmp, err := os.CreateTemp(d.dir, tempPrefix+"*")
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

	if err := d.publish(tmpName, name); err != nil {
		return "", "", err
	}

	return machine.DiskID(name), uuid, nil
}

// CreateRootFrom creates a root disk that is a sparse copy of the regular file
// at source, chowns it to the owner and returns the disk ID. The copy is made
// under a temporary name, synced, and published by rename; on any failure
// nothing is left in the directory.
func (d *Disk) CreateRootFrom(ctx context.Context, owner machine.DiskOwner, source string) (machine.DiskID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	name := string(machine.RootDiskID(owner.ID))
	if !validDiskName(name) {
		return "", fmt.Errorf("invalid disk name %q: must not contain / and must not be . or ..", name)
	}
	src, err := os.Open(source)
	if err != nil {
		return "", fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	info, err := src.Stat()
	if err != nil {
		return "", fmt.Errorf("stat source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("source %s is not a regular file", source)
	}
	tmp, err := os.CreateTemp(d.dir, tempPrefix+"*")
	if err != nil {
		return "", fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(what string, err error) (machine.DiskID, error) {
		return "", errors.Join(fmt.Errorf("%s: %w", what, err), tmp.Close(), os.Remove(tmpName))
	}

	if err := sparsecopy.Copy(tmp, src); err != nil {
		return fail("copy", err)
	}
	if err := tmp.Chown(int(owner.UID), int(owner.GID)); err != nil {
		return fail("chown", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return "", errors.Join(fmt.Errorf("close temp: %w", err), os.Remove(tmpName))
	}
	if err := d.publish(tmpName, name); err != nil {
		return "", err
	}
	if err := syncDir(d.dir); err != nil {
		return "", errors.Join(fmt.Errorf("sync directory: %w", err), os.Remove(filepath.Join(d.dir, name)))
	}
	return machine.DiskID(name), nil
}

// publish makes the staged file the disk of the given name, without replacing
// a disk that has the name: it returns machine.ErrDiskExists for one. The
// staged file is gone after it.
func (d *Disk) publish(staged, name string) error {
	err := os.Link(staged, filepath.Join(d.dir, name))
	_ = os.Remove(staged)
	switch {
	case errors.Is(err, os.ErrExist):
		return machine.ErrDiskExists
	case err != nil:
		return fmt.Errorf("publish: %w", err)
	}
	return nil
}

// Create makes a sparse raw file of the entry's size in the directory,
// formats it ext4 with a random UUID, records the request ID in an extended
// attribute and publishes it, syncing the directory. On any failure nothing is
// left in the directory.
func (d *Disk) Create(ctx context.Context, entry machine.DiskEntry) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validDiskID(entry.ID) {
		return fmt.Errorf("%w: %q must not be empty, ., .., start with %s or contain /", machine.ErrDiskInvalid, entry.ID, tempPrefix)
	}
	if entry.SizeBytes > math.MaxInt64 {
		return fmt.Errorf("%w: the size %d is too large", machine.ErrDiskInvalid, entry.SizeBytes)
	}
	uuid, err := randomUUID()
	if err != nil {
		return fmt.Errorf("generate uuid: %w", err)
	}
	tmp, err := os.CreateTemp(d.dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(what string, err error) error {
		return errors.Join(fmt.Errorf("%s: %w", what, err), tmp.Close(), os.Remove(tmpName))
	}
	if err := tmp.Truncate(int64(entry.SizeBytes)); err != nil {
		return fail("set size", err)
	}
	if err := runMKFS(d.mkfs, tmpName, uuid); err != nil {
		return fail("format", err)
	}
	if err := unix.Fsetxattr(int(tmp.Fd()), requestAttr, []byte(entry.Request), 0); err != nil {
		return fail("record the request", err)
	}
	if err := tmp.Sync(); err != nil {
		return fail("sync", err)
	}
	if err := tmp.Close(); err != nil {
		return errors.Join(fmt.Errorf("close temp: %w", err), os.Remove(tmpName))
	}
	if err := d.publish(tmpName, string(entry.ID)); err != nil {
		return err
	}
	if err := syncDir(d.dir); err != nil {
		return errors.Join(fmt.Errorf("sync directory: %w", err), os.Remove(filepath.Join(d.dir, string(entry.ID))))
	}
	return nil
}

// Get returns the disk named by the identifier, or machine.ErrDiskAbsent.
func (d *Disk) Get(_ context.Context, disk machine.DiskID) (machine.DiskEntry, error) {
	if !validDiskID(disk) {
		return machine.DiskEntry{}, fmt.Errorf("%w: %q must not be empty, ., .., start with %s or contain /", machine.ErrDiskInvalid, disk, tempPrefix)
	}
	path := filepath.Join(d.dir, string(disk))
	info, err := os.Lstat(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return machine.DiskEntry{}, machine.ErrDiskAbsent
	case err != nil:
		return machine.DiskEntry{}, err
	case !info.Mode().IsRegular():
		return machine.DiskEntry{}, machine.ErrDiskAbsent
	}
	request, err := readRequest(path)
	if err != nil {
		return machine.DiskEntry{}, err
	}
	return machine.DiskEntry{ID: disk, Request: request, SizeBytes: uint64(info.Size())}, nil
}

// readRequest returns the request ID recorded on the disk file, empty for a
// disk that has none, as a root disk.
func readRequest(path string) (machine.RequestID, error) {
	buf := make([]byte, 1024)
	n, err := unix.Getxattr(path, requestAttr, buf)
	switch {
	case errors.Is(err, unix.ENODATA):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the request of %s: %w", path, err)
	}
	return machine.RequestID(buf[:n]), nil
}

// List returns every regular file of the directory as a disk, ordered by
// identifier.
func (d *Disk) List(ctx context.Context) ([]machine.DiskEntry, error) {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return nil, fmt.Errorf("read the disk directory: %w", err)
	}
	var out []machine.DiskEntry
	for _, e := range entries {
		if !e.Type().IsRegular() || strings.HasPrefix(e.Name(), tempPrefix) {
			continue
		}
		switch entry, err := d.Get(ctx, machine.DiskID(e.Name())); {
		case err == nil:
			out = append(out, entry)
		case errors.Is(err, machine.ErrDiskAbsent):
			// Removed since the directory was read.
		default:
			return nil, err
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// syncDir fsyncs the directory at path, making a rename in it durable.
func syncDir(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

// validDiskName checks that name is safe as a path element: no /, not . or ..
func validDiskName(name string) bool {
	if name == "." || name == ".." {
		return false
	}
	return !bytes.ContainsRune([]byte(name), '/')
}

// validDiskID checks that disk is safe to use in a path under a directory:
// not empty, not . or .., not starting with the prefix of the files disks are
// built in, and no /.
func validDiskID(id machine.DiskID) bool {
	s := string(id)
	if s == "" || s == "." || s == ".." || strings.HasPrefix(s, tempPrefix) {
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
