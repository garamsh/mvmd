package firecracker

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	// stateName and memoryName are the files Snapshot has the VMM write in the
	// run directory. They are the same on every Snapshot of a Machine, so the
	// files of a failed attempt are written over by the next.
	stateName  = "snapshot.state"
	memoryName = "snapshot.mem"
	// inState and inMemory are where a restored VMM sees the two files in its
	// root, the same for every Machine, as ADR 0019's paths are. They are this
	// Driver's own: no other party names them.
	inState  = "/snapshot/state"
	inMemory = "/snapshot/memory"
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

// DescribeRestore implements machine.Driver. The files are the two Snapshot
// returned, wherever the Snapshot Store holds them now, and are told apart by
// name. The VMM sees them read-only at their fixed paths beside the Machine's
// own devices at theirs; it is given no kernel or initrd, since a restored
// guest is already running one. The tap of the boot description is kept for
// Restore, which is given the Machine's identifier alone.
func (d *Driver) DescribeRestore(_ context.Context, id machine.MachineID, spec machine.BootSpec, files []string) (machine.ProcessDescription, error) {
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
		{Host: state, In: inState, ReadOnly: true},
		{Host: memory, In: inMemory, ReadOnly: true},
	}
	p, err := d.process(spec, append(boot, machine.DeviceFiles(spec.Devices)...))
	if err != nil {
		return machine.ProcessDescription{}, err
	}
	d.mu.Lock()
	d.restoring[id] = spec.Network.Tap
	d.mu.Unlock()
	return p, nil
}

// Restore implements machine.Driver. It loads the snapshot DescribeRestore
// described into the started VMM and resumes the guest. The snapshot's network
// interface is attached to the tap the Machine was given, and the guest keeps
// the MAC the snapshot recorded. The description is spent by the call, whether
// or not it succeeds: Firecracker takes a load on a fresh process only, and a
// failed one leaves a VMM the lifecycle ends, so a repeat needs a new
// DescribeRestore.
func (d *Driver) Restore(ctx context.Context, id machine.MachineID) error {
	d.mu.Lock()
	tap, described := d.restoring[id]
	delete(d.restoring, id)
	d.mu.Unlock()
	if !described {
		return fmt.Errorf("no restore of Machine %q was described", id)
	}
	c, err := d.started(ctx, id)
	if err != nil {
		return err
	}
	load := map[string]any{
		"snapshot_path": inState,
		"mem_backend":   map[string]any{"backend_type": "File", "backend_path": inMemory},
		"resume_vm":     true,
	}
	if tap != "" {
		load["network_overrides"] = []map[string]any{{"iface_id": netIface, "host_dev_name": tap}}
	}
	return c.do(ctx, "PUT", "/snapshot/load", load, nil)
}
