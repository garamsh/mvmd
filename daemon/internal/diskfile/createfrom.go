package diskfile

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/sparsecopy"
)

// fromSnapshotAttr is the extended attribute that holds the disk snapshot a
// disk was made from, so a repeated create is compared against it (ADR 0056). A
// file without it was not made from one.
const fromSnapshotAttr = "user.mvmd.from_snapshot"

// readFromSnapshot returns the disk snapshot recorded on the disk file, empty
// for a disk not made from one.
func readFromSnapshot(path string) (machine.DiskSnapshotID, error) {
	buf := make([]byte, 1024)
	n, err := unix.Getxattr(path, fromSnapshotAttr, buf)
	switch {
	case errors.Is(err, unix.ENODATA):
		return "", nil
	case err != nil:
		return "", fmt.Errorf("read the disk snapshot of %s: %w", path, err)
	}
	return machine.DiskSnapshotID(buf[:n]), nil
}

// CreateFrom publishes a copy of the file at source, grown to the entry's size
// and recording its request, filesystem and snapshot. On failure nothing is
// left in the directory.
func (d *Disk) CreateFrom(ctx context.Context, entry machine.DiskEntry, source string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validDiskID(entry.ID) {
		return fmt.Errorf("%w: %q must not be empty, ., .., start with %s, contain / or a NUL, or exceed 255 bytes", machine.ErrDiskInvalid, entry.ID, tempPrefix)
	}
	if entry.SizeBytes > math.MaxInt64 {
		return fmt.Errorf("%w: the size %d is too large", machine.ErrDiskInvalid, entry.SizeBytes)
	}
	fs := entry.Filesystem
	if fs == "" {
		fs = machine.FilesystemExt4
	}
	if fs != machine.FilesystemNone && fs != machine.FilesystemExt4 {
		return fmt.Errorf("%w: filesystem %q is neither none nor ext4", machine.ErrDiskInvalid, fs)
	}
	src, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open source: %w", err)
	}
	defer src.Close()
	info, err := src.Stat()
	switch {
	case err != nil:
		return fmt.Errorf("stat source: %w", err)
	case !info.Mode().IsRegular():
		return fmt.Errorf("source %s is not a regular file", source)
	case uint64(info.Size()) > entry.SizeBytes:
		return fmt.Errorf("%w: %d bytes is smaller than the source's %d", machine.ErrDiskInvalid, entry.SizeBytes, info.Size())
	}
	tmp, err := os.CreateTemp(d.dir, tempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	fail := func(what string, err error) error {
		return errors.Join(fmt.Errorf("%s: %w", what, err), tmp.Close(), os.Remove(tmpName))
	}
	if _, err := sparsecopy.Duplicate(tmp, src); err != nil {
		return fail("duplicate", err)
	}
	if err := tmp.Truncate(int64(entry.SizeBytes)); err != nil {
		return fail("set size", err)
	}
	for _, a := range []struct {
		name  string
		value string
		what  string
	}{
		{requestAttr, string(entry.Request), "record the request"},
		{filesystemAttr, string(fs), "record the filesystem"},
		{fromSnapshotAttr, string(entry.FromSnapshot), "record the disk snapshot"},
	} {
		if err := unix.Fsetxattr(int(tmp.Fd()), a.name, []byte(a.value), 0); err != nil {
			return fail(a.what, err)
		}
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
