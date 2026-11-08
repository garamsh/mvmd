package machine

import (
	"context"
	"errors"
)

// ErrDiskAbsent is what Disk.Locate returns for a disk the node does not hold.
var ErrDiskAbsent = errors.New("disk not held by the node")

// Disk is the conversation with the node's disk files. Which Machine holds a
// disk is the inside's rule and not this port's.
type Disk interface {
	// Locate returns the path of a disk the node holds, or ErrDiskAbsent.
	Locate(ctx context.Context, disk DiskID) (path string, err error)
	// CreateRoot creates a Machine's root disk at the given size, formatted,
	// and returns its identifier.
	CreateRoot(ctx context.Context, id MachineID, sizeBytes uint64) (DiskID, error)
	// Remove deletes a disk file. Removing an absent disk succeeds.
	Remove(ctx context.Context, disk DiskID) error
}
