package memory

import (
	"context"
	"sort"
	"sync"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// Disk is an in-memory machine.Disk. Its steps are "locate", "create-root",
// "create-root-from", "create", "get", "list", "give", "reclaim" and "remove".
type Disk struct {
	Failures

	mu     sync.Mutex
	disks  map[machine.DiskID]uint64
	owner  map[machine.DiskID]machine.DiskOwner
	source map[machine.DiskID]string
	// request is the request ID of each disk Create made.
	request map[machine.DiskID]machine.RequestID
}

// NewDisk returns a Disk that holds no disk.
func NewDisk() *Disk {
	return &Disk{disks: make(map[machine.DiskID]uint64), owner: make(map[machine.DiskID]machine.DiskOwner), source: make(map[machine.DiskID]string), request: make(map[machine.DiskID]machine.RequestID)}
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
	disk := machine.RootDiskID(owner.ID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, held := d.disks[disk]; held {
		return "", "", machine.ErrDiskExists
	}
	d.disks[disk] = sizeBytes
	d.owner[disk] = owner
	return disk, "ext4-uuid-of-" + string(disk), nil
}

// CreateRootFrom implements machine.Disk.
func (d *Disk) CreateRootFrom(_ context.Context, owner machine.DiskOwner, source string) (machine.DiskID, error) {
	if err := d.Enter("create-root-from"); err != nil {
		return "", err
	}
	disk := machine.RootDiskID(owner.ID)
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, held := d.disks[disk]; held {
		return "", machine.ErrDiskExists
	}
	d.disks[disk] = 0
	d.owner[disk] = owner
	d.source[disk] = source
	return disk, nil
}

// Create implements machine.Disk.
func (d *Disk) Create(_ context.Context, entry machine.DiskEntry) error {
	if err := d.Enter("create"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, held := d.disks[entry.ID]; held {
		return machine.ErrDiskExists
	}
	d.disks[entry.ID] = entry.SizeBytes
	d.request[entry.ID] = entry.Request
	return nil
}

// Get implements machine.Disk.
func (d *Disk) Get(_ context.Context, disk machine.DiskID) (machine.DiskEntry, error) {
	if err := d.Enter("get"); err != nil {
		return machine.DiskEntry{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	size, held := d.disks[disk]
	if !held {
		return machine.DiskEntry{}, machine.ErrDiskAbsent
	}
	return machine.DiskEntry{ID: disk, Request: d.request[disk], SizeBytes: size}, nil
}

// List implements machine.Disk.
func (d *Disk) List(_ context.Context) ([]machine.DiskEntry, error) {
	if err := d.Enter("list"); err != nil {
		return nil, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]machine.DiskEntry, 0, len(d.disks))
	for disk, size := range d.disks {
		out = append(out, machine.DiskEntry{ID: disk, Request: d.request[disk], SizeBytes: size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Give implements machine.Disk.
func (d *Disk) Give(_ context.Context, disk machine.DiskID, owner machine.DiskOwner) error {
	if err := d.Enter("give"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, held := d.disks[disk]; !held {
		return machine.ErrDiskAbsent
	}
	d.owner[disk] = owner
	return nil
}

// Reclaim implements machine.Disk.
func (d *Disk) Reclaim(_ context.Context, disk machine.DiskID) error {
	if err := d.Enter("reclaim"); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, held := d.disks[disk]; !held {
		return machine.ErrDiskAbsent
	}
	delete(d.owner, disk)
	return nil
}

// Source returns the file a root disk made by CreateRootFrom was copied from.
func (d *Disk) Source(disk machine.DiskID) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.source[disk]
	return s, ok
}

// Remove implements machine.Disk.
func (d *Disk) Remove(ctx context.Context, disk machine.DiskID) error {
	if err := d.Enter("remove"); err != nil {
		return err
	}
	// A removal whose context has ended by the time it is made removes nothing.
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	delete(d.disks, disk)
	delete(d.owner, disk)
	delete(d.source, disk)
	delete(d.request, disk)
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

// Owner reports the DiskOwner of a disk a Machine owns, as CreateRoot or Give
// made it, and false for a disk mvmd owns.
func (d *Disk) Owner(disk machine.DiskID) (machine.DiskOwner, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o, ok := d.owner[disk]
	return o, ok
}
