package machine

import (
	"context"
	"errors"
	"time"
)

// ErrDiskSnapshotAbsent is what DiskSnapshotStore.Get returns for a disk
// snapshot the node does not hold.
var ErrDiskSnapshotAbsent = errors.New("disk snapshot not held by the node")

// ErrDiskSnapshotExists is what DiskSnapshotStore.Put returns for an identifier
// that already names a disk snapshot.
var ErrDiskSnapshotExists = errors.New("disk snapshot already held by the node")

// DiskSnapshotID is the client-chosen identifier of a disk snapshot. It obeys
// the rules of a disk identifier (ValidDiskID).
type DiskSnapshotID string

// DiskSnapshot is the record of a disk snapshot: the disk it was taken of,
// which may no longer exist, the request that made it, and what a disk made
// from it takes (ADR 0056).
type DiskSnapshot struct {
	ID      DiskSnapshotID
	Disk    DiskID
	Request RequestID
	// SizeBytes is the length of the disk's file when it was copied, and the
	// least a disk made from the snapshot has.
	SizeBytes uint64
	// Filesystem is what the file held, which a disk made from the snapshot
	// holds.
	Filesystem Filesystem
	CreatedAt  time.Time
}

// DiskSnapshotInfo is a disk snapshot's record and what the store holds of it.
type DiskSnapshotInfo struct {
	DiskSnapshot
	// AllocatedBytes is the disk space the snapshot's file occupies: allocated
	// blocks, so a sparse or shared file counts what it holds and not its length.
	AllocatedBytes uint64
}

// DiskSnapshotStore is the conversation with the node's disk snapshots: one
// file each and its record.
type DiskSnapshotStore interface {
	// Put copies the file at source and records the snapshot, all or nothing, or
	// returns ErrDiskSnapshotExists. It never changes the source.
	Put(ctx context.Context, snapshot DiskSnapshot, source string) error
	// Get returns the snapshot and the path of its file, or
	// ErrDiskSnapshotAbsent. The file is read and never changed.
	Get(ctx context.Context, id DiskSnapshotID) (DiskSnapshot, string, error)
	// List returns every disk snapshot the store holds, ordered by identifier.
	List(ctx context.Context) ([]DiskSnapshotInfo, error)
	// Remove deletes the snapshot and its file. Removing an absent snapshot
	// succeeds.
	Remove(ctx context.Context, id DiskSnapshotID) error
}
