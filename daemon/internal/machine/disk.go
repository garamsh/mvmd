package machine

import (
	"context"
	"errors"
)

// ErrDiskAbsent is what Disk.Locate returns for a disk the node does not hold.
var ErrDiskAbsent = errors.New("disk not held by the node")

// ErrDiskExists is what Disk.Create, CreateRoot and CreateRootFrom return for
// an identifier a disk file already has.
var ErrDiskExists = errors.New("disk already held by the node")

// ErrDiskInvalid is what Disk.Create returns for an identifier that cannot
// name a disk file.
var ErrDiskInvalid = errors.New("not a disk identifier")

// DiskEntry is a disk the node holds as the Disk adapter records it: the
// request that made it, which a root disk has none of, and its size.
type DiskEntry struct {
	ID        DiskID
	Request   RequestID
	SizeBytes uint64
}

// DiskInfo is a disk the node holds as ListDisks reports it: its size and the
// Machine that holds it, if any.
type DiskInfo struct {
	ID        DiskID
	SizeBytes uint64
	Holder    MachineID
}

// Disk is the conversation with the node's disk files. Which Machine holds a
// disk is the inside's rule and not this port's.
type Disk interface {
	// Locate returns the path of a disk the node holds, or ErrDiskAbsent.
	Locate(ctx context.Context, disk DiskID) (path string, err error)
	// CreateRoot creates a Machine's root disk at the given size, formatted,
	// and returns its identifier and filesystem UUID, or ErrDiskExists.
	CreateRoot(ctx context.Context, owner DiskOwner, sizeBytes uint64) (DiskID, string, error)
	// CreateRootFrom creates a Machine's root disk as a copy of the disk file
	// at source, owned as CreateRoot's is, and returns its identifier, or ErrDiskExists.
	CreateRootFrom(ctx context.Context, owner DiskOwner, source string) (DiskID, error)
	// Create makes a disk of the entry's size, formatted as a root disk is and
	// owned by mvmd's own user, and records its request ID with it, all or
	// nothing. It returns ErrDiskExists for an identifier a disk file has, and
	// ErrDiskInvalid for one that cannot name a file; on any other error no disk
	// is left.
	Create(ctx context.Context, entry DiskEntry) error
	// Get returns the disk the node holds, or ErrDiskAbsent.
	Get(ctx context.Context, disk DiskID) (DiskEntry, error)
	// List returns every disk the node holds, root disks included, ordered by
	// identifier.
	List(ctx context.Context) ([]DiskEntry, error)
	// Remove deletes a disk file. Removing an absent disk succeeds.
	Remove(ctx context.Context, disk DiskID) error
}
