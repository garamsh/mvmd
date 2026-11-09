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
// mkfs.ext4 fails the adapter's check, does not start. runDirs is the
// supervision directory, which holds the Machines' run directories.
func realStorage(cfg Config, runDirs string) (machine.Disk, machine.SnapshotStore, error) {
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
	if err := checkSameFileSystem(runDirs, filepath.Join(cfg.StateDir, "snapshots", "staging")); err != nil {
		return nil, nil, err
	}
	return disk, snapshots, nil
}

// checkSameFileSystem probes the operation a Snapshot's Put depends on: taking
// a Driver's state file, which lives in a run directory, into the Snapshot
// Store's staging directory by rename (runtime.md §State and stability). A
// rename fails with EXDEV across file systems, even across two mounts of one,
// which a comparison of device numbers would not see.
func checkSameFileSystem(runDirs, staging string) error {
	probe, err := os.CreateTemp(runDirs, ".mvmd-rename-probe-")
	if err != nil {
		return fmt.Errorf("the supervision directory %s: %w", runDirs, err)
	}
	from := probe.Name()
	_ = probe.Close()
	to := filepath.Join(staging, filepath.Base(from))
	if err := os.Rename(from, to); err != nil {
		_ = os.Remove(from)
		return fmt.Errorf("the snapshot directory %s is not on the file system of the supervision directory %s, so a Put could not take a Machine's state files in by rename: %w", staging, runDirs, err)
	}
	if err := os.Remove(to); err != nil {
		return fmt.Errorf("the snapshot staging directory %s: %w", staging, err)
	}
	return nil
}
