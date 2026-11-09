//go:build firecracker_real

package firecracker_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/channel"
	"github.com/garamsh/mvmd/daemon/internal/firecracker"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// runDirOf names the run directory of each Machine of a test.
type runDirOf map[machine.MachineID]string

func (r runDirOf) RunDir(id machine.MachineID) (string, error) {
	if dir, ok := r[id]; ok {
		return dir, nil
	}
	return "", fmt.Errorf("no run directory for Machine %q", id)
}

// startVMM runs the described process in its root, as the supervisor would, and
// ends it when the test does.
func startVMM(t *testing.T, p machine.ProcessDescription, run string) {
	t.Helper()
	vmmLog, err := os.Create(filepath.Join(run, "vmm.log"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = vmmLog.Close() })
	cmd, err := inRoot(p)
	require.NoError(t, err)
	cmd.Stdout, cmd.Stderr = vmmLog, vmmLog
	require.NoError(t, cmd.Start())
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		<-exited
		if t.Failed() {
			if raw, err := os.ReadFile(filepath.Join(run, "vmm.log")); err == nil {
				t.Logf("the VMM's log (%s):\n%s", run, raw)
			}
		}
	})
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	raw, err := os.ReadFile(from)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(to, raw, 0o600))
}

// A Machine is booted, paused and snapshotted; the snapshot is then restored as
// a new Machine, with a run directory and a root disk of its own at other host
// paths than the first's, and the Guest Agent answers on it. The restored
// VMM's root holds no kernel and no initrd, and the files the snapshot
// recorded are at the fixed paths of the second Machine's own.
func TestReal_ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn(t *testing.T) {
	// The Unix socket paths bound the length of this directory.
	dir, err := os.MkdirTemp("", "fcsnap")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	runs := runDirOf{}
	for _, id := range []machine.MachineID{"m1", "m2"} {
		runs[id] = filepath.Join(dir, "run-"+string(id))
		require.NoError(t, os.Mkdir(runs[id], 0o700))
	}
	store := filepath.Join(dir, "store")
	require.NoError(t, os.Mkdir(store, 0o700))

	d, err := firecracker.New(firecracker.Config{
		Binary: input(t, "MVMD_FIRECRACKER"), BinarySHA256: input(t, "MVMD_FIRECRACKER_SHA256"),
		Kernel: input(t, "MVMD_KERNEL"), KernelSHA256: input(t, "MVMD_KERNEL_SHA256"),
		Initrd: input(t, "MVMD_INITRD"),
		Dirs:   runs,
	})
	require.NoError(t, err)
	require.Contains(t, d.Info().Capabilities, machine.CapabilitySnapshot)

	devices := make([]machine.BlockDevice, 0, 2)
	for i, name := range []string{"image.img", "root.img"} {
		path := filepath.Join(dir, name)
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Truncate(path, 16<<20))
		devices = append(devices, machine.BlockDevice{Disk: machine.DiskID(name), Path: path, ReadOnly: i == 0})
	}
	spec := machine.BootSpec{Devices: devices, Resources: machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}, RunDir: runs["m1"]}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	startVMM(t, p.For(spec), runs["m1"])
	require.NoError(t, d.Boot(ctx, "m1", spec))

	guest := channel.NewGuest(d)
	answers := func(id machine.MachineID, within time.Duration) func() bool {
		return func() bool {
			cctx, ccancel := context.WithTimeout(ctx, within)
			defer ccancel()
			return guest.Health(cctx, id) == nil
		}
	}
	require.Eventually(t, answers("m1", 5*time.Second), time.Minute, 500*time.Millisecond, "the Guest Agent never answered on the first Machine")

	// The lifecycle pauses the guest for the capture. A second snapshot of the
	// same Machine writes the same files over the first's.
	require.NoError(t, d.Pause(ctx, "m1"))
	first, err := d.Snapshot(ctx, "m1")
	require.NoError(t, err)
	second, err := d.Snapshot(ctx, "m1")
	require.NoError(t, err)
	assert.Equal(t, first, second, "two snapshots of one Machine name the same files")
	assert.Equal(t, input(t, "MVMD_KERNEL_SHA256"), first.KernelDigest)
	require.Len(t, first.Files, 2)
	for _, f := range first.Files {
		assert.Equal(t, runs["m1"], filepath.Dir(f), "the files are in the run directory")
		info, err := os.Stat(f)
		require.NoError(t, err)
		assert.Positive(t, info.Size(), "%s is empty", f)
	}
	require.NoError(t, d.Resume(ctx, "m1"))
	require.Eventually(t, answers("m1", 5*time.Second), 30*time.Second, 200*time.Millisecond, "the snapshotted Machine did not answer after it was resumed")

	// What the Snapshot Store does: take the state files in and copy the root
	// disk, so the restored Machine's files are all at new paths.
	var held []string
	for _, f := range first.Files {
		to := filepath.Join(store, filepath.Base(f))
		require.NoError(t, os.Rename(f, to))
		held = append(held, to)
	}
	root2 := filepath.Join(dir, "root2.img")
	copyFile(t, devices[1].Path, root2)
	spec2 := machine.BootSpec{
		Devices:   []machine.BlockDevice{devices[0], {Disk: "root2", Path: root2}},
		Resources: spec.Resources, RunDir: runs["m2"],
	}

	p2, err := d.DescribeRestore(ctx, "m2", spec2, held)
	require.NoError(t, err)
	startVMM(t, p2.For(spec2), runs["m2"])
	_, err = d.GuestConn(ctx, "m2", machine.GuestAgentPort)
	require.Error(t, err, "control: a VMM that has loaded nothing has no guest to reach")

	require.NoError(t, d.Restore(ctx, "m2", spec2))
	require.Eventually(t, answers("m2", 5*time.Second), time.Minute, 500*time.Millisecond, "the Guest Agent never answered on the restored Machine")
	assert.Eventually(t, answers("m1", 5*time.Second), 10*time.Second, 200*time.Millisecond, "the first Machine is unaffected by its restore")

	// The restored VMM is its own process, with its own sockets.
	for _, id := range []machine.MachineID{"m1", "m2"} {
		_, err := os.Stat(filepath.Join(runs[id], "vsock.sock"))
		assert.NoError(t, err, "%s has its own vsock socket", id)
	}
}
