package machine

import (
	"context"
	"errors"
	"time"
)

// ErrDiskAbsent is what Disk.Locate, Give and Reclaim return for a disk the node does not hold.
var ErrDiskAbsent = errors.New("disk not held by the node")

// ErrDiskExists is what Disk.Create, CreateRoot and CreateRootFrom return for
// an identifier a disk file already has.
var ErrDiskExists = errors.New("disk already held by the node")

// ErrDiskInvalid is what Disk.Create returns for an identifier that cannot
// name a disk file.
var ErrDiskInvalid = errors.New("not a disk identifier")

// RootDiskID is the identifier of a Machine's root disk. The inside holds the
// name before it asks the Disk port to publish the file, so no call removes the
// disk between the two.
func RootDiskID(id MachineID) DiskID { return DiskID(string(id) + "-root") }

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

// DiskFile is what a disk's file shows of it: its size and the time it was
// last written. A write through a guest changes the latter, and a give or a take
// does not.
type DiskFile struct {
	SizeBytes uint64
	ModTime   time.Time
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
	// Stat returns the size and modification time of the disk's file, or
	// ErrDiskAbsent.
	Stat(ctx context.Context, disk DiskID) (DiskFile, error)
	// Give makes the disk file owned by the owner's user, so the VMM of the
	// Machine that holds the disk can open it, or returns ErrDiskAbsent.
	Give(ctx context.Context, disk DiskID, owner DiskOwner) error
	// Reclaim makes the disk file owned by mvmd's own user again, or returns
	// ErrDiskAbsent.
	Reclaim(ctx context.Context, disk DiskID) error
	// Remove deletes a disk file. Removing an absent disk succeeds.
	Remove(ctx context.Context, disk DiskID) error
}
