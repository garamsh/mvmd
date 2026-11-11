package firecracker

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	// stateName and memoryName are the files Snapshot has the VMM write in the
	// run directory. They are the same on every Snapshot of a Machine, so the
	// files of a failed attempt are written over by the next.
	stateName  = "snapshot.state"
	memoryName = "snapshot.mem"
	// inStateName and inMemoryName are the names of the two files in
	// machine.InSnapshotDir, where a restored VMM sees them (ADR 0019).
	inStateName  = "state"
	inMemoryName = "memory"
	// netIface is the interface Boot names, and so the one a snapshot records.
	netIface = "eth0"
)

// Snapshot implements machine.Driver. The Machine is paused, as the lifecycle
// holds it: Firecracker refuses to snapshot a guest that is running. The files
// are written by the VMM, as its user, at paths in its root that are the run
// directory's, and returned at the host's path of that directory. The guest
// kernel's digest is the pinned one, which New checked and the supervisor
// checks again at every start.
func (d *Driver) Snapshot(ctx context.Context, id machine.MachineID) (machine.DriverSnapshot, error) {
	dir, err := d.cfg.Dirs.RunDir(id)
	if err != nil {
		return machine.DriverSnapshot{}, err
	}
	c := newClient(filepath.Join(dir, apiSocketName))
	err = c.do(ctx, "PUT", "/snapshot/create", map[string]any{
		"snapshot_type": "Full",
		"snapshot_path": inRun(stateName),
		"mem_file_path": inRun(memoryName),
	}, nil)
	if err != nil {
		return machine.DriverSnapshot{}, err
	}
	return machine.DriverSnapshot{
		Files:        []string{filepath.Join(dir, stateName), filepath.Join(dir, memoryName)},
		KernelDigest: d.cfg.KernelSHA256,
	}, nil
}

// bootDevices is how many of a restore's devices the Machine boots with: its
// image and its root disk, at /disk/0 and /disk/1. Each after them is a disk
// the snapshot recorded.
const bootDevices = 2

// DescribeRestore implements machine.Driver. The files are the two Snapshot
// returned, wherever the Snapshot Store holds them now, and are told apart by
// name. The VMM sees them read-only at their fixed paths beside the Machine's
// own devices at theirs; it is given no kernel or initrd, since a restored
// guest is already running one. Each recorded disk is a run file under its
// drive's name, where the snapshot recorded it (ADR 0042), and is not bound.
func (d *Driver) DescribeRestore(_ context.Context, _ machine.MachineID, spec machine.BootSpec, files []string) (machine.ProcessDescription, error) {
	var state, memory string
	for _, f := range files {
		switch {
		case !filepath.IsAbs(f):
			return machine.ProcessDescription{}, fmt.Errorf("the snapshot file %q is not an absolute path", f)
		case filepath.Base(f) == stateName && state == "":
			state = f
		case filepath.Base(f) == memoryName && memory == "":
			memory = f
		default:
			return machine.ProcessDescription{}, fmt.Errorf("the snapshot file %q is not the one state file and one memory file a snapshot holds", f)
		}
	}
	if state == "" || memory == "" {
		return machine.ProcessDescription{}, fmt.Errorf("a snapshot holds a state file %s and a memory file %s, and %d files were given", stateName, memoryName, len(files))
	}
	boot := []machine.File{
		{Host: state, In: inSnapshot(inStateName), ReadOnly: true},
		{Host: memory, In: inSnapshot(inMemoryName), ReadOnly: true},
	}
	own, recorded := spec.Devices, []machine.BlockDevice(nil)
	if len(own) > bootDevices {
		own, recorded = own[:bootDevices], own[bootDevices:]
	}
	p, err := d.process(spec, append(boot, machine.DeviceFiles(own)...))
	if err != nil {
		return machine.ProcessDescription{}, err
	}
	for _, dev := range recorded {
		_, _ = dev, diskdevice.Name(0, dev.Disk)
	}
	return p, nil
}

// Restore implements machine.Driver. It loads the snapshot DescribeRestore
// described into the started VMM and resumes the guest. The snapshot's network
// interface is attached to the tap the Machine was given, and the guest keeps
// the MAC the snapshot recorded. Firecracker takes a load on a fresh process
// only, so after a failed one the lifecycle ends the VMM and a repeat is made
// on a new process.
func (d *Driver) Restore(ctx context.Context, id machine.MachineID, spec machine.BootSpec) error {
	c, err := d.started(ctx, id)
	if err != nil {
		return err
	}
	load := map[string]any{
		"snapshot_path": inSnapshot(inStateName),
		"mem_backend":   map[string]any{"backend_type": "File", "backend_path": inSnapshot(inMemoryName)},
		"resume_vm":     true,
	}
	if tap := spec.Network.Tap; tap != "" {
		load["network_overrides"] = []map[string]any{{"iface_id": netIface, "host_dev_name": tap}}
	}
	return c.do(ctx, "PUT", "/snapshot/load", load, nil)
}
