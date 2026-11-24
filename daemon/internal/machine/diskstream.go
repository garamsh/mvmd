package machine

import (
	"context"
	"errors"
	"regexp"
)

// DiskReadSource names what ReadDisk reads: a disk or a disk snapshot, one of
// them.
type DiskReadSource struct {
	Disk     DiskID
	Snapshot DiskSnapshotID
}

// DiskSink receives a ReadDisk stream: the header first, then the extents in
// ascending order, then the digest.
type DiskSink interface {
	Header(sizeBytes uint64, filesystem Filesystem) error
	Extent(offset uint64, data []byte) error
	Digest(sha256 string) error
}

// DiskExtents yields the extents of a disk a client sends: Next returns the
// next run of bytes with its offset, and io.EOF after the last. The data is
// valid until the next call.
type DiskExtents interface {
	Next() (offset uint64, data []byte, err error)
}

var diskDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

func (s *service) ReadDisk(ctx context.Context, source DiskReadSource, sink DiskSink) error {
	switch {
	case source.Disk != "" && source.Snapshot != "":
		return fail(ErrInvalid, "a disk and a disk snapshot are both named: name one")
	case source.Disk == "" && source.Snapshot == "":
		return fail(ErrInvalid, "neither a disk nor a disk snapshot is named")
	case source.Snapshot != "":
		return s.readDiskSnapshot(ctx, source.Snapshot, sink)
	case !ValidDiskID(source.Disk):
		return invalidDisk(source.Disk)
	}
	for range resizeAttempts {
		if done, err := s.readDisk(ctx, source.Disk, sink); done {
			return err
		}
	}
	return fail(ErrPreconditionFailed, "the holder of disk %q kept changing", source.Disk)
}

func (s *service) readDiskSnapshot(ctx context.Context, snapshot DiskSnapshotID, sink DiskSink) error {
	if !ValidDiskID(DiskID(snapshot)) {
		return InvalidDiskSnapshot(snapshot)
	}
	if err := s.requireDiskSnapshots(); err != nil {
		return err
	}
	// Shared until the read has ended, so a delete of the snapshot is refused
	// and a SnapshotDisk still writing it is waited for.
	defer s.catalog.HoldDiskSnapshot(snapshot, false)()
	snap, path, err := s.diskSnaps.Get(ctx, snapshot)
	switch {
	case errors.Is(err, ErrDiskSnapshotAbsent):
		return fail(ErrNotFound, "disk snapshot %q is not held by the node", snapshot)
	case err != nil:
		return fail(ErrFailed, "look up disk snapshot %q: %v", snapshot, err)
	}
	return s.stream(ctx, path, snap.SizeBytes, snap.Filesystem, sink)
}

// readDisk is one attempt of ReadDisk on a disk. It reports done false when the
// holder changed under it and the call should look again.
func (s *service) readDisk(ctx context.Context, disk DiskID, sink DiskSink) (done bool, _ error) {
	s.mu.Lock()
	holder, held := s.held[disk]
	s.mu.Unlock()
	if !held {
		// The holder slot keeps an attach from taking the disk, and a resize, a
		// snapshot or a delete from changing it, while it is read.
		if _, _, ok := s.claim(reading, disk); !ok {
			return false, nil
		}
		defer s.release(disk)
		return true, s.streamDisk(ctx, disk, sink)
	}
	if holder == deleting || holder == resizing || holder == snapshotting || holder == reading {
		return true, fail(ErrPreconditionFailed, "disk %q is %s", disk, holderIs(holder))
	}
	// The Machine's lock is held to the end of the read, so no start finds the
	// disk changing under it.
	defer s.lock(holder)()
	s.mu.Lock()
	still := s.held[disk] == holder
	s.mu.Unlock()
	if !still {
		return false, nil
	}
	rec, err := s.machine(holder)
	if err != nil {
		return true, err
	}
	if st := rec.Machine.State; st != StateCreated && st != StateStopped {
		return true, fail(ErrPreconditionFailed, "disk %q is held by %s Machine %q; only the disk of a Created or Stopped Machine is read", disk, st, holder)
	}
	return true, s.streamDisk(ctx, disk, sink)
}

func (s *service) streamDisk(ctx context.Context, disk DiskID, sink DiskSink) error {
	e, err := s.disks.Get(ctx, disk)
	switch {
	case errors.Is(err, ErrDiskAbsent):
		return fail(ErrNotFound, "disk %q is not held by the node", disk)
	case err != nil:
		return fail(ErrFailed, "read disk %q: %v", disk, err)
	}
	path, err := s.locate(ctx, disk)
	if err != nil {
		return err
	}
	return s.stream(ctx, path, e.SizeBytes, e.Filesystem, sink)
}

// stream sends the file at path to the sink. An error of the sink is returned
// as it came, which the caller reads as the stream's own.
func (s *service) stream(ctx context.Context, path string, sizeBytes uint64, fs Filesystem, sink DiskSink) error {
	if err := sink.Header(sizeBytes, fs); err != nil {
		return err
	}
	var sinkErr error
	sum, err := s.disks.ReadExtents(ctx, path, func(offset uint64, data []byte) error {
		sinkErr = sink.Extent(offset, data)
		return sinkErr
	})
	switch {
	case sinkErr != nil:
		return sinkErr
	case err != nil:
		return fail(ErrFailed, "read %s: %v", path, err)
	}
	return sink.Digest(sum)
}

func (s *service) CreateDiskFromStream(ctx context.Context, disk DiskID, request RequestID, sizeBytes uint64, filesystem Filesystem, digest string, extents DiskExtents) error {
	switch {
	case !ValidDiskID(disk):
		return invalidDisk(disk)
	case request == "":
		return fail(ErrInvalid, "the request ID is empty")
	case sizeBytes == 0:
		return fail(ErrInvalid, "the disk size is zero")
	case filesystem != FilesystemNone && filesystem != FilesystemExt4:
		return fail(ErrInvalid, "filesystem %q is neither %q nor %q", filesystem, FilesystemNone, FilesystemExt4)
	case !diskDigest.MatchString(digest):
		return fail(ErrInvalid, "the digest %q is not 64 lowercase hexadecimal digits", digest)
	}
	asked := DiskEntry{ID: disk, Request: request, SizeBytes: sizeBytes, Filesystem: filesystem, ContentsDigest: digest}
	defer s.diskLocks.Lock(disk, true)()
	// The caller's own context bounds the upload: a stream that breaks ends the
	// call with nothing published.
	for attempt := 0; ; attempt++ {
		switch have, err := s.disks.Get(ctx, disk); {
		case err == nil:
			if have == asked {
				return nil
			}
			return fail(ErrConflict, "disk %q exists under another request ID, size, filesystem or digest", disk)
		case !errors.Is(err, ErrDiskAbsent):
			return fail(ErrFailed, "look up disk %q: %v", disk, err)
		}
		err := s.disks.CreateFromExtents(ctx, asked, extents)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrDiskInvalid), errors.Is(err, ErrDiskStreamMismatch):
			return fail(ErrInvalid, "%v", err)
		case errors.Is(err, ErrDiskExists) && attempt == 0:
			// A call made the disk between the lookup and the create: what it
			// made is what the lookup now answers for.
		default:
			return fail(ErrFailed, "create disk %q from a stream: %v", disk, err)
		}
	}
}
