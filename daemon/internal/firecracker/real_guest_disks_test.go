//go:build firecracker_real

package firecracker_test

import (
	"context"
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

// A booted guest finds a disk the Driver hot-plugged, releases it before the
// Driver unplugs it, and finds it again when it is plugged back (ADR 0022).
// Firecracker tells the guest of neither: the Guest Agent rescans the PCI bus
// and removes the device itself, holding the device exclusively while it does.
// A repeated release succeeds, so a detach that failed half way can be
// retried. The agent serves both calls before it has been booted, which is the
// state the initrd's agent is in here.
func TestReal_AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug(t *testing.T) {
	dir, err := os.MkdirTemp("", "fcgdisk")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	run := filepath.Join(dir, "run")
	require.NoError(t, os.Mkdir(run, 0o700))

	d, err := firecracker.New(firecracker.Config{
		Binary: input(t, "MVMD_FIRECRACKER"), BinarySHA256: input(t, "MVMD_FIRECRACKER_SHA256"),
		Kernel: input(t, "MVMD_KERNEL"), KernelSHA256: input(t, "MVMD_KERNEL_SHA256"),
		Initrd: input(t, "MVMD_INITRD"),
		Dirs:   runDirs{run},
	})
	require.NoError(t, err)

	file := func(name string) machine.BlockDevice {
		path := filepath.Join(dir, name+".img")
		require.NoError(t, os.WriteFile(path, nil, 0o600))
		require.NoError(t, os.Truncate(path, 16<<20))
		return machine.BlockDevice{Disk: machine.DiskID(name), Path: path}
	}
	image, root := file("image"), file("root")
	image.Disk = ""
	spec := machine.BootSpec{
		Devices:   []machine.BlockDevice{image, root},
		Resources: machine.Resources{VCPUs: 2, MemoryBytes: 256 << 20}, RunDir: run,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	startVMM(t, p.For(spec), run)
	require.NoError(t, d.Boot(ctx, "m1", spec))

	guest := channel.NewGuest(d)
	within := func(f func(context.Context) error) error {
		cctx, ccancel := context.WithTimeout(ctx, 30*time.Second)
		defer ccancel()
		return f(cctx)
	}
	require.Eventually(t, func() bool {
		return within(func(c context.Context) error { return guest.Health(c, "m1") }) == nil
	}, time.Minute, 500*time.Millisecond, "the Guest Agent never answered")

	// Control: a disk nothing was plugged for is not found, and a release of a
	// disk never found is refused as unknown, so the repeats below that succeed
	// are the agent's record of a release and not an answer by default.
	err = within(func(c context.Context) error { return guest.FindDisk(c, "m1", "hot") })
	require.Error(t, err, "no new device has appeared")
	require.ErrorIs(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), machine.ErrDiskUnknown)

	hot := file("hot")
	sock := filepath.Join(run, "api.sock")
	for round := 1; round <= 2; round++ {
		require.NoError(t, d.AttachDisk(ctx, "m1", hot), "round %d", round)
		require.NoError(t, within(func(c context.Context) error { return guest.FindDisk(c, "m1", "hot") }), "the guest did not find the disk, round %d", round)
		require.NoError(t, within(func(c context.Context) error { return guest.FindDisk(c, "m1", "hot") }), "a repeat of the find, round %d", round)
		assert.Len(t, drivesOf(t, sock), 3, "round %d", round)

		require.NoError(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), "round %d", round)
		require.NoError(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), "a repeat after the release, round %d", round)
		require.NoError(t, d.DetachDisk(ctx, "m1", "hot"), "round %d", round)
		require.NoError(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), "a repeat after the unplug, round %d", round)
		assert.Len(t, drivesOf(t, sock), 2, "round %d", round)
	}
	assert.NoError(t, within(func(c context.Context) error { return guest.Health(c, "m1") }), "the guest still answers after the unplug")
}
