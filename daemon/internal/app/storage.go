package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/snapshotstore"
)

// realStorage constructs the Disk and the Snapshot Store of a node that runs
// the Firecracker Driver: root disks in "disks" and snapshots in "snapshots",
// both under the state directory (runtime.md §State and stability). Each input
// is checked here, so a node whose directories cannot be opened, or whose
// mkfs.ext4 fails the adapter's check, does not start.
func realStorage(cfg Config) (machine.Disk, machine.SnapshotStore, error) {
	// A root disk is owned by its Machine's VMM user and created by the
	// daemon, before the Machine's supervisor and its state directory exist,
	// so the disks share one directory that only the daemon can list.
	disks := filepath.Join(cfg.StateDir, "disks")
	if err := os.MkdirAll(disks, 0o700); err != nil {
		return nil, nil, fmt.Errorf("the disk directory: %w", err)
	}
	disk, err := diskfile.NewDisk(disks, cfg.Disk.MKFS)
	if err != nil {
		return nil, nil, fmt.Errorf("the Disk: %w", err)
	}
	snapshots, err := snapshotstore.New(filepath.Join(cfg.StateDir, "snapshots"))
	if err != nil {
		return nil, nil, fmt.Errorf("the Snapshot Store: %w", err)
	}
	return disk, snapshots, nil
}
