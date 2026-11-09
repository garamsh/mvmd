package drivertest

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/statedir"
)

// ADiskIsAttachedToARunningMachineAndDetachedAgain hot-plugs disks into a booted
// Machine and unplugs them again, and a disk the boot gave it as well. A Driver
// that reports no disk-hotplug capability is held to refusing the operations
// instead.
func ADiskIsAttachedToARunningMachineAndDetachedAgain(t *testing.T, s Subject) {
	dir := tempDir(t, "dtdisk")
	run := statedir.Run(dir)
	require.NoError(t, os.Mkdir(run, 0o700))
	d := s.Driver(t, RunDirs{"m1": run})

	image, root, given := diskFile(t, dir, "image", true), diskFile(t, dir, "root", false), diskFile(t, dir, "given", false)
	image.Disk = ""
	boot := []machine.BlockDevice{image, root, given}
	spec := machine.BootSpec{Devices: boot, Resources: resources, RunDir: run}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	startLogged(t, s, p.For(spec), run)
	require.NoError(t, d.Boot(ctx, "m1", spec))

	guest := s.Guest(d)
	answers := func() bool { return healthy(ctx, guest, "m1", 5*time.Second) }
	require.Eventually(t, answers, time.Minute, 500*time.Millisecond, "the Guest Agent never answered")

	hot, hotRO := diskFile(t, dir, "hot", false), diskFile(t, dir, "hotro", true)
	if !has(d, machine.CapabilityDiskHotplug) {
		assert.Empty(t, diskOperationsAccepted(ctx, d, "m1", hot),
			"a Driver that reports no disk-hotplug capability refuses these")
		return
	}
	s.check(t, Booted, Machine{ID: "m1", RunDir: run, Devices: boot})

	for _, dev := range []machine.BlockDevice{hot, hotRO} {
		require.NoError(t, d.AttachDisk(ctx, "m1", dev), dev.Disk)
	}
	attached := append(append([]machine.BlockDevice(nil), boot...), hot, hotRO)
	s.check(t, Attached, Machine{ID: "m1", RunDir: run, Devices: attached})
	assert.True(t, answers(), "the guest still answers with the disks attached")

	// Drive reads the VMM's configuration: a boot's drive and an added one.
	for disk, want := range map[machine.DiskID]machine.DriveState{
		"given": machine.DriveAtBoot, "hot": machine.DriveHotPlugged, "hotro": machine.DriveHotPlugged, "none": machine.DriveAbsent,
	} {
		got, err := d.Drive(ctx, "m1", disk)
		require.NoError(t, err, disk)
		assert.Equal(t, want, got, disk)
	}

	require.NoError(t, d.DetachDisk(ctx, "m1", "hot"))
	require.NoError(t, d.DetachDisk(ctx, "m1", "hotro"))
	require.NoError(t, d.DetachDisk(ctx, "m1", "given"))
	s.check(t, Detached, Machine{ID: "m1", RunDir: run, Devices: boot[:2]})
	for _, dev := range []machine.BlockDevice{hot, hotRO, given} {
		_, err := os.Stat(dev.Path)
		assert.NoError(t, err, "the file of %s is untouched", dev.Disk)
	}
	assert.True(t, answers(), "the guest still answers after the detach")
}

// AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug has the Driver
// hot-plug a disk, the Guest Agent find it and release it before the Driver
// unplugs it, and find it again when it is plugged back (ADR 0022). A repeated
// release succeeds, so a detach that failed half way can be retried. The agent
// serves both calls before it has been booted, which is the state the initrd's
// agent is in here. A Driver that reports no disk-hotplug capability is held to
// refusing the plug and the unplug instead.
func AGuestFindsAHotPluggedDiskAndReleasesItBeforeTheUnplug(t *testing.T, s Subject) {
	dir := tempDir(t, "dtgdisk")
	run := statedir.Run(dir)
	require.NoError(t, os.Mkdir(run, 0o700))
	d := s.Driver(t, RunDirs{"m1": run})

	image, root := diskFile(t, dir, "image", false), diskFile(t, dir, "root", false)
	image.Disk = ""
	boot := []machine.BlockDevice{image, root}
	spec := machine.BootSpec{Devices: boot, Resources: resources, RunDir: run}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	startLogged(t, s, p.For(spec), run)
	require.NoError(t, d.Boot(ctx, "m1", spec))

	guest := s.Guest(d)
	within := func(f func(context.Context) error) error {
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		return f(cctx)
	}
	require.Eventually(t, func() bool {
		return within(func(c context.Context) error { return guest.Health(c, "m1") }) == nil
	}, time.Minute, 500*time.Millisecond, "the Guest Agent never answered")

	hot := diskFile(t, dir, "hot", false)
	if !has(d, machine.CapabilityDiskHotplug) {
		assert.Empty(t, diskOperationsAccepted(ctx, d, "m1", hot),
			"a Driver that reports no disk-hotplug capability refuses these")
		return
	}

	// Control: a disk nothing was plugged for is not found, and a release of a
	// disk never found is refused as unknown, so the repeats below that succeed
	// are the agent's record of a release and not an answer by default.
	err = within(func(c context.Context) error { return guest.FindDisk(c, "m1", "hot") })
	require.Error(t, err, "no new device has appeared")
	require.ErrorIs(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), machine.ErrDiskUnknown)

	for round := 1; round <= 2; round++ {
		require.NoError(t, d.AttachDisk(ctx, "m1", hot), "round %d", round)
		s.check(t, Attached, Machine{ID: "m1", RunDir: run, Devices: append(append([]machine.BlockDevice(nil), boot...), hot)})
		require.NoError(t, within(func(c context.Context) error { return guest.FindDisk(c, "m1", "hot") }), "the guest did not find the disk, round %d", round)
		require.NoError(t, within(func(c context.Context) error { return guest.FindDisk(c, "m1", "hot") }), "a repeat of the find, round %d", round)

		require.NoError(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), "round %d", round)
		require.NoError(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), "a repeat after the release, round %d", round)
		require.NoError(t, d.DetachDisk(ctx, "m1", "hot"), "round %d", round)
		require.NoError(t, within(func(c context.Context) error { return guest.ReleaseDisk(c, "m1", "hot") }), "a repeat after the unplug, round %d", round)
		s.check(t, Detached, Machine{ID: "m1", RunDir: run, Devices: boot})
	}
	assert.NoError(t, within(func(c context.Context) error { return guest.Health(c, "m1") }), "the guest still answers after the unplug")
}

// diskOperationsAccepted names the disk operations the Driver accepted: the
// plug of a disk into the running Machine, the unplug, and the read of its
// drives.
func diskOperationsAccepted(ctx context.Context, d machine.Driver, id machine.MachineID, dev machine.BlockDevice) []string {
	var accepted []string
	if err := d.AttachDisk(ctx, id, dev); err == nil {
		accepted = append(accepted, "AttachDisk")
	}
	if err := d.DetachDisk(ctx, id, dev.Disk); err == nil {
		accepted = append(accepted, "DetachDisk")
	}
	if _, err := d.Drive(ctx, id, dev.Disk); err == nil {
		accepted = append(accepted, "Drive")
	}
	return accepted
}
