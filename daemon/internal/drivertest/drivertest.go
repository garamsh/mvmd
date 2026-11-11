// Package drivertest is the scenarios every machine.Driver is held to against a
// real VMM: a guest that boots and answers through GuestConn, a snapshot that
// restores as a new Machine, a disk that is attached to a running Machine and
// detached again, and a guest that finds a hot-plugged disk and releases it
// before the unplug. It is test-support, as nstest is: each Driver's real test
// is a thin caller under that Driver's own build tag, which builds a Subject
// and runs the scenarios.
//
// The suite names no VMM. What only one VMM can show, such as Firecracker's own
// list of drives, comes in through Subject.Check. A scenario for a capability
// the Driver does not report is not skipped: it asserts that the Driver refuses
// the operation.
package drivertest

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// RunDirs names the run directory of each Machine of a scenario. The suite makes
// the directories and gives the Driver the names, so a Subject builds its Driver
// over them.
type RunDirs map[machine.MachineID]string

// RunDir returns the run directory of the Machine.
func (r RunDirs) RunDir(id machine.MachineID) (string, error) {
	if dir, ok := r[id]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no run directory for Machine %q", id)
}

// Streams are the standard streams the VMM process is started with. A nil In is
// no standard input.
type Streams struct {
	In, Out, Err *os.File
}

// Process is the VMM process a Subject started.
type Process interface {
	// Kill ends the process.
	Kill() error
	// Exited is closed when the process has ended.
	Exited() <-chan struct{}
}

// Checkpoint is a point of a scenario at which the Subject may look at its VMM.
type Checkpoint int

const (
	// Booted is a booted Machine, whose Devices are the boot's.
	Booted Checkpoint = iota
	// Attached is after disks were attached, whose Devices are every disk the
	// Machine now holds.
	Attached
	// Detached is after disks were detached, whose Devices are the ones left.
	Detached
	// Restored is each of two Machines, one restored from the other's snapshot,
	// both running.
	Restored
)

// Machine is what a Subject's check is told of the Machine it looks at.
type Machine struct {
	ID     machine.MachineID
	RunDir string
	// Devices are the disks the Machine should now hold, in the order of the
	// boot and then of the attach.
	Devices []machine.BlockDevice
}

// Subject is a Driver and what a run needs from its caller.
type Subject struct {
	// Driver builds the Driver under test over the run directories.
	Driver func(t *testing.T, dirs RunDirs) machine.Driver
	// Guest reaches the Guest Agent of a Machine through the Driver.
	Guest func(d machine.Driver) machine.Guest
	// Start runs the described process as the supervisor would, with the streams,
	// and returns it. The suite ends it when the scenario does.
	Start func(t *testing.T, p machine.ProcessDescription, s Streams) Process
	// Kernel is the guest kernel the suite boots: the file the Subject's Driver
	// is configured with, and its pinned digest.
	Kernel machine.KernelFile
	// Snapshot is what only the Subject's VMM can say of its snapshot. It is
	// read only when the Driver reports the snapshot capability.
	Snapshot SnapshotFacts
	// Check is what only the Subject's VMM can tell, run at each Checkpoint. It
	// may be nil.
	Check func(t *testing.T, at Checkpoint, m Machine)
}

// SnapshotFacts are what differs between the snapshots of two VMMs, which the
// scenario holds a Driver to exactly, so that it is no weaker for either.
type SnapshotFacts struct {
	// Files is how many files a snapshot holds.
	Files int
	// RestoreBootFiles are the paths, in the restored VMM's root, of the boot
	// files its VMM opens on restore: the kernel and initrd paths of a boot that
	// the restore binds, and no others. Firecracker opens none; Cloud Hypervisor
	// opens the initrd.
	RestoreBootFiles []string
	// LoadsOnRestore is true when the VMM loads the snapshot in Restore, so that
	// before it the process has no guest. It is false when the process loads it
	// as it starts, from its command line.
	LoadsOnRestore bool
}

func (s Subject) check(t *testing.T, at Checkpoint, m Machine) {
	t.Helper()
	if s.Check != nil {
		s.Check(t, at, m)
	}
}

func has(d machine.Driver, c machine.Capability) bool {
	for _, have := range d.Info().Capabilities {
		if have == c {
			return true
		}
	}
	return false
}

// resources are the size of every Machine of the suite.
var resources = machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}

// diskFile makes a 16 MiB raw disk file in dir.
func diskFile(t *testing.T, dir, name string, readOnly bool) machine.BlockDevice {
	t.Helper()
	path := filepath.Join(dir, name+".img")
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	require.NoError(t, os.Truncate(path, 16<<20))
	return machine.BlockDevice{Disk: machine.DiskID(name), Path: path, ReadOnly: readOnly}
}

// tempDir makes a directory whose path a Unix socket address holds.
func tempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// longTempDir makes a directory whose path is over the 107 bytes a Unix socket
// address holds, so that a socket in it is reachable only through a descriptor
// of the directory.
func longTempDir(t *testing.T, prefix string) string {
	t.Helper()
	dir := filepath.Join(tempDir(t, prefix), strings.Repeat("d", 100))
	require.NoError(t, os.Mkdir(dir, 0o755))
	return dir
}

// startVMM starts the described process with the streams and ends it when the
// test does, logging the VMM's output if the test failed.
func startVMM(t *testing.T, s Subject, p machine.ProcessDescription, streams Streams, logPath string) Process {
	t.Helper()
	proc := s.Start(t, p, streams)
	t.Cleanup(func() {
		_ = proc.Kill()
		<-proc.Exited()
		if t.Failed() {
			if raw, err := os.ReadFile(logPath); err == nil {
				t.Logf("the VMM's log (%s):\n%s", logPath, raw)
			}
		}
	})
	return proc
}

// startLogged starts the process with its output in the run directory's log and
// no standard input.
func startLogged(t *testing.T, s Subject, p machine.ProcessDescription, run string) Process {
	t.Helper()
	path := filepath.Join(run, "vmm.log")
	log, err := os.Create(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = log.Close() })
	return startVMM(t, s, p, Streams{Out: log, Err: log}, path)
}

// healthy reports whether the Guest Agent answers within the time.
func healthy(ctx context.Context, g machine.Guest, id machine.MachineID, within time.Duration) bool {
	cctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()
	return g.Health(cctx, id) == nil
}

// awaitOutput reads the session until it holds want, or fails after the time.
func awaitOutput(r io.Reader, want string, within time.Duration) error {
	done := make(chan error, 1)
	var seen bytes.Buffer
	go func() {
		buf := make([]byte, 1024)
		for !bytes.Contains(seen.Bytes(), []byte(want)) {
			n, err := r.Read(buf)
			seen.Write(buf[:n])
			if err != nil {
				done <- fmt.Errorf("the session ended before %q: %w (saw %q)", want, err, seen.String())
				return
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(within):
		return fmt.Errorf("no %q within %v", want, within)
	}
}
