package machine

import (
	"context"
	"errors"
	"time"
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
	// KernelName is the name the Machine recorded for its kernel; empty for a
	// Machine that recorded none (ADR 0045).
	KernelName string
}

// SnapshotSpec is what a snapshot keeps of a Machine's Spec: the image, the
// resources, their ceiling and the root disk size, and the memory its VMM booted
// with, which a live resize of the restored Machine needs (ADR 0029). It has no
// field for a network policy, since a snapshot's Machine is new, with a policy
// of its own.
type SnapshotSpec struct {
	Image           ImageDigest
	Resources       Resources
	Ceiling         Resources
	BootMemoryBytes uint64
	RootDiskBytes   uint64
}

// SnapshotDisk is a disk its Machine had attached when the snapshot was taken,
// and what its file showed then. A restore takes the disk only while its file
// shows the same (ADR 0041).
type SnapshotDisk struct {
	Disk      DiskID
	ReadOnly  bool
	SizeBytes uint64
	ModTime   time.Time
}

// Snapshot is the record of a snapshot: the Machine it came from, the request
// that made it, what it takes to boot it again and what it is compatible with.
type Snapshot struct {
	ID      SnapshotID
	Machine MachineID
	Request RequestID
	Spec    SnapshotSpec
	// RootDiskUUID is the filesystem UUID of the root disk the guest was
	// running on, which a restored guest still expects.
	RootDiskUUID string
	// MAC is the guest's MAC, which a restored guest keeps (ADR 0011).
	MAC    string
	Compat SnapshotCompat
	// Disks are the disks the Machine had attached, in the order it listed
	// them. A snapshot made before they were recorded has none.
	Disks []SnapshotDisk
	// CreatedAt is when the snapshot was stored, written into its record by
	// Put. It is the zero time in a record written before it existed.
	CreatedAt time.Time
}

// SnapshotInfo is a snapshot's record and what the store holds of it.
type SnapshotInfo struct {
	Snapshot
	// SizeBytes is the disk space the store's files for the snapshot occupy:
	// allocated blocks, summed over every file in its directory.
	SizeBytes uint64
}

// SnapshotFiles are the files a snapshot holds: the Driver's own, opaque to
// the inside, and a copy of the Machine's root disk, so that memory and disk
// are one pair.
type SnapshotFiles struct {
	// State are the files the Driver wrote in the run directory. The store
	// may move them. A Driver names them the same on every Snapshot of a
	// Machine, so the files a failed attempt left are written over by the
	// next one.
	State []string
	// RootDisk is the path of the Machine's own root disk, which is running. The
	// store copies it and never moves or changes it.
	RootDisk string
}

// SnapshotStore is the conversation with the node's snapshots: the files of
// each and its record.
type SnapshotStore interface {
	// Put takes the files in and records the snapshot, all or nothing, or
	// returns ErrSnapshotExists. It copies RootDisk and may move State; on an
	// error nothing of the snapshot is left, and State is left where it was.
	Put(ctx context.Context, snapshot Snapshot, files SnapshotFiles) error
	// Get returns the snapshot and where its files are now, or
	// ErrSnapshotAbsent.
	Get(ctx context.Context, id SnapshotID) (Snapshot, SnapshotFiles, error)
	// List returns every snapshot the store holds, ordered by identifier. A
	// snapshot being put or removed at the time is either in it or not.
	List(ctx context.Context) ([]SnapshotInfo, error)
	// Remove deletes the snapshot and its files. Removing an absent snapshot
	// succeeds.
	Remove(ctx context.Context, id SnapshotID) error
}
