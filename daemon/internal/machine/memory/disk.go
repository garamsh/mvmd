package memory

import (
	"context"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Disk is an in-memory machine.Disk. Its steps are "locate", "create-root",
// "create-root-from" and "remove".
type Disk struct {
	Failures

	mu     sync.Mutex
	disks  map[machine.DiskID]uint64
	owner  map[machine.DiskID]machine.DiskOwner
	source map[machine.DiskID]string
}

// NewDisk returns a Disk that holds no disk.
func NewDisk() *Disk {
	return &Disk{disks: make(map[machine.DiskID]uint64), owner: make(map[machine.DiskID]machine.DiskOwner), source: make(map[machine.DiskID]string)}
}

// Provide makes the node hold a disk, as how one comes to exist is not the
// lifecycle's concern.
func (d *Disk) Provide(disk machine.DiskID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.disks[disk] = 0
}

// Locate implements machine.Disk.
func (d *Disk) Locate(_ context.Context, disk machine.DiskID) (string, error) {
	if err := d.Enter("locate"); err != nil {
		return "", err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.disks[disk]; !ok {
		return "", machine.ErrDiskAbsent
	}
	return "/disks/" + string(disk), nil
}

// CreateRoot implements machine.Disk.
func (d *Disk) CreateRoot(_ context.Context, owner machine.DiskOwner, sizeBytes uint64) (machine.DiskID, string, error) {
	if err := d.Enter("create-root"); err != nil {
		return "", "", err
	}
	disk := machine.DiskID(string(owner.ID) + "-root")
	d.mu.Lock()
	d.disks[disk] = sizeBytes
	d.owner[disk] = owner
	d.mu.Unlock()
	return disk, "", nil
}

// CreateRootFrom implements machine.Disk.
func (d *Disk) CreateRootFrom(_ context.Context, owner machine.DiskOwner, source string) (machine.DiskID, error) {
	if err := d.Enter("create-root-from"); err != nil {
		return "", err
	}
	disk := machine.DiskID(string(owner.ID) + "-root")
	d.mu.Lock()
	d.disks[disk] = 0
	d.owner[disk] = owner
	d.source[disk] = source
	d.mu.Unlock()
	return disk, nil
}

// Source returns the file a root disk made by CreateRootFrom was copied from.
func (d *Disk) Source(disk machine.DiskID) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.source[disk]
	return s, ok
}

// Remove implements machine.Disk.
func (d *Disk) Remove(_ context.Context, disk machine.DiskID) error {
	if err := d.Enter("remove"); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.disks, disk)
	delete(d.owner, disk)
	delete(d.source, disk)
	d.mu.Unlock()
	return nil
}

// Holds reports whether the node holds the disk, and its size if it was made
// by CreateRoot.
func (d *Disk) Holds(disk machine.DiskID) (size uint64, held bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	size, held = d.disks[disk]
	return size, held
}

// Owner reports the DiskOwner for a disk created by CreateRoot.
func (d *Disk) Owner(disk machine.DiskID) (machine.DiskOwner, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.owner[disk]
	return o, ok
}
