package app

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/diskfile"
	"github.com/garamsh/mvmd/daemon/internal/disksnapshotstore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/snapshotstore"
)

// realStorage constructs the Disk and the Snapshot Store of a node that runs
// a real Driver: root disks in "disks", disk snapshots in "disk-snapshots"
// and snapshots in "snapshots",
// both under the state directory (runtime.md §State and stability). Each input
// is checked here, so a node whose directories cannot be opened, or whose
// mkfs.ext4 fails the adapter's check, does not start. runDirs is the
// supervision directory, which holds the Machines' run directories.
func realStorage(cfg Config, runDirs string) (machine.Disk, machine.SnapshotStore, machine.DiskSnapshotStore, error) {
	// A root disk is owned by its Machine's VMM user and created by the
	// daemon, before the Machine's supervisor and its state directory exist,
	// so the disks share one directory that only the daemon can list.
	disks := filepath.Join(cfg.StateDir, "disks")
	if err := os.MkdirAll(disks, 0o700); err != nil {
		return nil, nil, nil, fmt.Errorf("the disk directory: %w", err)
	}
	disk, err := diskfile.NewDisk(disks, cfg.Disk.MKFS)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the Disk: %w", err)
	}
	snapshots, err := snapshotstore.New(filepath.Join(cfg.StateDir, "snapshots"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the Snapshot Store: %w", err)
	}
	if err := checkSameFileSystem(runDirs, filepath.Join(cfg.StateDir, "snapshots", "staging")); err != nil {
		return nil, nil, nil, err
	}
	diskSnapshots, err := disksnapshotstore.New(filepath.Join(cfg.StateDir, "disk-snapshots"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("the Disk Snapshot Store: %w", err)
	}
	if err := checkDisksSameFileSystem(disks, filepath.Join(cfg.StateDir, "disk-snapshots", "staging")); err != nil {
		return nil, nil, nil, err
	}
	return disk, snapshots, diskSnapshots, nil
}

// checkSameFileSystem probes the operation a Snapshot's Put depends on: taking
// a Driver's state file, which lives in a run directory, into the Snapshot
// Store's staging directory by rename (runtime.md §State and stability). A
// rename fails with EXDEV across file systems, even across two mounts of one,
// which a comparison of device numbers would not see.
func checkSameFileSystem(runDirs, staging string) error {
	// The supervision adapter makes the directory with its first Machine, so
	// it may not exist yet; it makes it the same way.
	if err := os.MkdirAll(runDirs, 0o700); err != nil {
		return fmt.Errorf("the supervision directory %s: %w", runDirs, err)
	}
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

// checkDisksSameFileSystem probes what a disk snapshot depends on: a copy of
// a disk is a clone where the file system has reflinks, and a clone is made
// within one file system (runtime.md §State and stability). A hard link fails
// with EXDEV across file systems, even across two mounts of one.
func checkDisksSameFileSystem(disks, staging string) error {
	probe, err := os.CreateTemp(disks, "tmp-probe-")
	if err != nil {
		return fmt.Errorf("the disk directory %s: %w", disks, err)
	}
	from := probe.Name()
	_ = probe.Close()
	defer func() { _ = os.Remove(from) }()
	to := filepath.Join(staging, filepath.Base(from))
	if err := os.Link(from, to); err != nil {
		return fmt.Errorf("the disk snapshot directory %s is not on the file system of the disk directory %s, so a disk could not be cloned into a snapshot: %w", staging, disks, err)
	}
	if err := os.Remove(to); err != nil {
		return fmt.Errorf("the disk snapshot staging directory %s: %w", staging, err)
	}
	return nil
}
