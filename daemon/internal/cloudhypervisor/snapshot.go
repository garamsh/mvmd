package cloudhypervisor

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// The three files of a snapshot, named by the VMM: it writes them into the
// directory it is given and reads them from the one it restores from
// (vmm/src/migration/mod.rs and vmm/src/memory_manager.rs at v54.0).
const (
	configName = "config.json"
	stateName  = "state.json"
	memoryName = "memory-ranges"
)

// snapshotNames are the files in the order Snapshot returns them.
var snapshotNames = []string{configName, stateName, memoryName}

// Snapshot implements machine.Driver. The Machine is paused, as the lifecycle
// holds it. The VMM writes the three files into the run directory, as its user,
// under the names above, which are the same on every Snapshot of a Machine. It
// creates them and refuses one that exists, so the files of an earlier Snapshot,
// or of a failed attempt, are removed first. They are returned at the run
// directory's host path. The guest kernel's digest is the pinned one, which New
// checked and the supervisor checks again at every start.
func (d *Driver) Snapshot(ctx context.Context, id machine.MachineID) (machine.DriverSnapshot, error) {
	dir, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return machine.DriverSnapshot{}, err
	}
	files := make([]string, 0, len(snapshotNames))
	for _, name := range snapshotNames {
		path := filepath.Join(dir, name)
		if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return machine.DriverSnapshot{}, fmt.Errorf("remove the earlier snapshot file: %v", err)
		}
		files = append(files, path)
	}
	c := newClient(filepath.Join(dir, apiSocketName))
	if err := c.do(ctx, "PUT", "/vm.snapshot", map[string]string{"destination_url": "file://" + machine.InRunDir}, nil); err != nil {
		return machine.DriverSnapshot{}, err
	}
	return machine.DriverSnapshot{Files: files, KernelDigest: d.cfg.KernelSHA256}, nil
}

// DescribeRestore implements machine.Driver. The files are the three Snapshot
// returned, wherever the Snapshot Store holds them now, and are told apart by
// name. The VMM sees them read-only in machine.InSnapshotDir, and the Machine's
// own devices at their fixed paths as at boot. It is given no kernel, which a
// restored guest has already loaded, but it is given the initrd, with its digest: Cloud
// Hypervisor opens the initramfs its snapshot's configuration names before it
// restores (ADR 0027), and never loads it. The restore is the process's own
// command line, so Restore only waits for it. Each recorded disk is a run file
// under its device's name, where the snapshot recorded it (ADR 0042), and is
// not bound.
func (d *Driver) DescribeRestore(_ context.Context, _ machine.MachineID, spec machine.BootSpec, files []string) (machine.ProcessDescription, error) {
	if err := validate(spec); err != nil {
		return machine.ProcessDescription{}, err
	}
	held := map[string]string{}
	for _, f := range files {
		name := filepath.Base(f)
		switch {
		case !filepath.IsAbs(f):
			return machine.ProcessDescription{}, fmt.Errorf("the snapshot file %q is not an absolute path", f)
		case !isSnapshotName(name) || held[name] != "":
			return machine.ProcessDescription{}, fmt.Errorf("the snapshot file %q is not one of the %s a snapshot holds, once each", f, strings.Join(snapshotNames, ", "))
		}
		held[name] = f
	}
	boot := []machine.File{d.initrd()}
	for _, name := range snapshotNames {
		if held[name] == "" {
			return machine.ProcessDescription{}, fmt.Errorf("a snapshot holds %s, and %d files were given without it", strings.Join(snapshotNames, ", "), len(files))
		}
		boot = append(boot, machine.File{Host: held[name], In: filepath.Join(machine.InSnapshotDir, name), ReadOnly: true})
	}
	p := d.process(spec, restoreArgs(spec), boot)
	for _, dev := range spec.RecordedDisks {
		_, _ = dev, diskdevice.Name(0, dev.Disk)
	}
	return p, nil
}

func isSnapshotName(name string) bool {
	for _, n := range snapshotNames {
		if n == name {
			return true
		}
	}
	return false
}

// restoreArgs is the command line that restores the VM: the snapshot's own
// configuration is the VM's, so no device is named, and the guest resumes. The
// new Machine's tap is the descriptor the supervisor passes, given to the
// network device the snapshot recorded by its id. The guest keeps the MAC the
// snapshot recorded.
func restoreArgs(spec machine.BootSpec) []string {
	restore := "source_url=file://" + machine.InSnapshotDir + ",resume=true"
	if spec.Network.Tap != "" {
		restore += fmt.Sprintf(",net_fds=[%s@[%d]]", netID, tapDescriptor)
	}
	return []string{
		"--api-socket", "path=" + inRun(apiSocketName),
		"--log-file", inRun(logName),
		"--event-monitor", fmt.Sprintf("fd=%d", eventDescriptor),
		"--restore", restore,
	}
}

// Restore implements machine.Driver. The process DescribeRestore described
// restores the VM as it starts, so this waits as Boot does: for the API socket,
// for a VMM that is the release this Driver is written against, and for the VM
// to be Running. A restore the VMM refuses ends the VMM, which is what the wait
// then reports. The spec is not read: it is what DescribeRestore was given.
func (d *Driver) Restore(ctx context.Context, id machine.MachineID, _ machine.BootSpec) error {
	return d.awaitRunning(ctx, id)
}
