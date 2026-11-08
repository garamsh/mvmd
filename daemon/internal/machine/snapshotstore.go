package machine

import (
	"context"
	"errors"
)

// ErrSnapshotAbsent is what SnapshotStore.Get returns for a snapshot the node
// does not hold.
var ErrSnapshotAbsent = errors.New("snapshot not held by the node")

// ErrSnapshotExists is what SnapshotStore.Put returns for an identifier that
// already names a snapshot.
var ErrSnapshotExists = errors.New("snapshot already held by the node")

// SnapshotID is the client-chosen identifier of a snapshot.
type SnapshotID string

// SnapshotCompat is what a snapshot records of the node that made it, and what
// a restore is checked against. A snapshot does not move to another VMM, to
// another version of it or to another CPU model (driver.md).
type SnapshotCompat struct {
	Driver       string
	Version      string
	CPUModel     string
	KernelDigest string
}

// Snapshot is the record of a snapshot: the Machine it came from, the request
// that made it, what it takes to boot it again and what it is compatible with.
// It carries no network policy and no attached disk: a snapshot's Machine is
// new, with a policy of its own and no disk but its root.
type Snapshot struct {
	ID      SnapshotID
	Machine MachineID
	Request RequestID
	// Spec holds the image, the resources and the root disk size.
	Spec Spec
	// RootDiskUUID is the filesystem UUID of the root disk the guest was
	// running on, which a restored guest still expects.
	RootDiskUUID string
	Compat       SnapshotCompat
}

// SnapshotFiles are the files a snapshot holds: the Driver's own, opaque to
// the inside, and a copy of the Machine's root disk, so that memory and disk
// are one pair.
type SnapshotFiles struct {
	State    []string
	RootDisk string
}

// SnapshotStore is the conversation with the node's snapshots: the files of
// each and its record.
type SnapshotStore interface {
	// Put takes the files in and records the snapshot, all or nothing, or
	// returns ErrSnapshotExists. The caller's files are copied or moved as the
	// store sees fit and are no longer the caller's.
	Put(ctx context.Context, snapshot Snapshot, files SnapshotFiles) error
	// Get returns the snapshot and where its files are now, or
	// ErrSnapshotAbsent.
	Get(ctx context.Context, id SnapshotID) (Snapshot, SnapshotFiles, error)
	// Remove deletes the snapshot and its files. Removing an absent snapshot
	// succeeds.
	Remove(ctx context.Context, id SnapshotID) error
}
