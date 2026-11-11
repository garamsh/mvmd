package drivertest

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/diskdevice"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn boots a Machine,
// pauses it and snapshots it; the snapshot is then restored as a new Machine,
// with a run directory and a root disk of its own at other host paths than the
// first's, and the Guest Agent answers on it. The restored VMM's root holds no
// kernel, and holds exactly the boot files its VMM opens on restore (the
// Subject's facts: none for Firecracker, the initrd for Cloud Hypervisor), at
// the host paths the first Machine booted them from. The files the snapshot
// recorded are at the fixed paths of the second Machine's own.
//
// A Driver that reports no snapshot capability is held to refusing the
// operations instead.
func ASnapshotRestoresAsANewMachineThatAnswersThroughGuestConn(t *testing.T, s Subject) {
	// The Unix socket paths bound the length of this directory.
	dir := tempDir(t, "dtsnap")
	runs := RunDirs{}
	for _, id := range []machine.MachineID{"m1", "m2"} {
		runs[id] = filepath.Join(dir, "run-"+string(id))
		require.NoError(t, os.Mkdir(runs[id], 0o700))
	}
	store := filepath.Join(dir, "store")
	require.NoError(t, os.Mkdir(store, 0o700))
	d := s.Driver(t, runs)

	devices := []machine.BlockDevice{diskFile(t, dir, "image", true), diskFile(t, dir, "root", false)}
	spec := machine.BootSpec{Devices: devices, Resources: resources, RunDir: runs["m1"]}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	startLogged(t, s, p.For(spec), runs["m1"])
	require.NoError(t, d.Boot(ctx, "m1", spec))
	booted := p.BootFiles

	guest := s.Guest(d)
	answers := func(id machine.MachineID, within time.Duration) func() bool {
		return func() bool { return healthy(ctx, guest, id, within) }
	}
	require.Eventually(t, answers("m1", 5*time.Second), time.Minute, 500*time.Millisecond, "the Guest Agent never answered on the first Machine")

	if !has(d, machine.CapabilitySnapshot) {
		require.NoError(t, d.Pause(ctx, "m1"))
		refusesSnapshot(ctx, t, d, "m1", spec)
		return
	}

	// The lifecycle pauses the guest for the capture. A second snapshot of the
	// same Machine writes the same files over the first's.
	require.NoError(t, d.Pause(ctx, "m1"))
	first, err := d.Snapshot(ctx, "m1")
	require.NoError(t, err)
	second, err := d.Snapshot(ctx, "m1")
	require.NoError(t, err)
	assert.Equal(t, first, second, "two snapshots of one Machine name the same files")
	assert.Equal(t, s.KernelDigest, first.KernelDigest)
	require.Positive(t, s.Snapshot.Files, "the Subject states how many files its snapshot holds")
	require.Len(t, first.Files, s.Snapshot.Files)
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
	raw, err := os.ReadFile(devices[1].Path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(root2, raw, 0o600))
	spec2 := machine.BootSpec{
		Devices:   []machine.BlockDevice{devices[0], {Disk: "root2", Path: root2}},
		Resources: spec.Resources, RunDir: runs["m2"],
	}

	p2, err := d.DescribeRestore(ctx, "m2", spec2, held)
	require.NoError(t, err)
	assert.ElementsMatch(t, s.Snapshot.RestoreBootFiles, bootPaths(p2.BootFiles),
		"the restored root holds the boot files its VMM opens on restore, and no others")
	for _, in := range s.Snapshot.RestoreBootFiles {
		assert.NotEmpty(t, hostOf(p2.BootFiles, in), "control: %s is bound", in)
		assert.Equal(t, hostOf(booted, in), hostOf(p2.BootFiles, in), "%s is the file the first Machine booted from", in)
	}
	startLogged(t, s, p2.For(spec2), runs["m2"])
	if s.Snapshot.LoadsOnRestore {
		_, err = d.GuestConn(ctx, "m2", machine.GuestAgentPort)
		require.Error(t, err, "control: a VMM that has loaded nothing has no guest to reach")
	}

	require.NoError(t, d.Restore(ctx, "m2", spec2))
	require.Eventually(t, answers("m2", 5*time.Second), time.Minute, 500*time.Millisecond, "the Guest Agent never answered on the restored Machine")
	assert.Eventually(t, answers("m1", 5*time.Second), 10*time.Second, 200*time.Millisecond, "the first Machine is unaffected by its restore")

	// The restored VMM is its own process, with its own sockets, which only the
	// VMM's own files can show.
	s.check(t, Restored, Machine{ID: "m1", RunDir: runs["m1"], Devices: devices})
	s.check(t, Restored, Machine{ID: "m2", RunDir: runs["m2"], Devices: spec2.Devices})
}

// ARestoredMachineHoldsTheDiskItsSnapshotRecordedAsAHotPluggedDrive attaches a
// disk to a booted Machine, snapshots it and ends it, removing its run
// directory with the link the attach made, and restores the snapshot as a new
// Machine whose disk is the same file. The restore's description lists the disk
// as a run file under its device's name and does not bind it, the process the
// description names finds the file at the path the snapshot recorded (ADR 0042),
// and the restored VMM answers that it holds the disk as one added since boot
// (ADR 0041). The disk's file is as the snapshot found it, which is what a
// restore through the service compares.
//
// A Driver that reports no disk-hotplug or no snapshot capability is held to
// refusing the operations instead.
func ARestoredMachineHoldsTheDiskItsSnapshotRecordedAsAHotPluggedDrive(t *testing.T, s Subject) {
	dir := tempDir(t, "dtrdisk")
	runs := RunDirs{}
	for _, id := range []machine.MachineID{"m1", "m2"} {
		runs[id] = filepath.Join(dir, "run-"+string(id))
		require.NoError(t, os.Mkdir(runs[id], 0o700))
	}
	store := filepath.Join(dir, "store")
	require.NoError(t, os.Mkdir(store, 0o700))
	d := s.Driver(t, runs)

	image, root, data := diskFile(t, dir, "image", true), diskFile(t, dir, "root", false), diskFile(t, dir, "data", false)
	image.Disk = ""
	devices := []machine.BlockDevice{image, root}
	spec := machine.BootSpec{Devices: devices, Resources: resources, RunDir: runs["m1"]}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	p, err := d.Describe(ctx, "m1", spec)
	require.NoError(t, err)
	first := startLogged(t, s, p.For(spec), runs["m1"])
	require.NoError(t, d.Boot(ctx, "m1", spec))

	guest := s.Guest(d)
	answers := func(id machine.MachineID) bool { return healthy(ctx, guest, id, 5*time.Second) }
	require.Eventually(t, func() bool { return answers("m1") }, time.Minute, 500*time.Millisecond, "the Guest Agent never answered on the first Machine")

	if !has(d, machine.CapabilityDiskHotplug) {
		refusesDisks(ctx, t, d, "m1", data)
		return
	}
	if !has(d, machine.CapabilitySnapshot) {
		require.NoError(t, d.Pause(ctx, "m1"))
		refusesSnapshot(ctx, t, d, "m1", spec)
		return
	}

	require.NoError(t, d.AttachDisk(ctx, "m1", data))
	link := filepath.Join(runs["m1"], diskdevice.Name(0, data.Disk))
	_, err = os.Stat(link)
	require.NoError(t, err, "setup: the attach linked the disk into the run directory")
	require.NoError(t, d.Pause(ctx, "m1"))
	taken, err := d.Snapshot(ctx, "m1")
	require.NoError(t, err)
	captured, err := os.Stat(data.Path)
	require.NoError(t, err)

	// What the Snapshot Store and a Delete do: the state files go into the
	// store, the root disk is copied, and the VMM and the run directory, with
	// the attach's link, go away. The disk's file is all that is left of it.
	var held []string
	for _, f := range taken.Files {
		to := filepath.Join(store, filepath.Base(f))
		require.NoError(t, os.Rename(f, to))
		held = append(held, to)
	}
	root2 := filepath.Join(dir, "root2.img")
	raw, err := os.ReadFile(root.Path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(root2, raw, 0o600))
	require.NoError(t, first.Kill())
	<-first.Exited()
	require.NoError(t, os.RemoveAll(runs["m1"]))
	_, err = os.Lstat(link)
	require.ErrorIs(t, err, os.ErrNotExist, "setup: the disk has no link left in any run directory")

	spec2 := machine.BootSpec{
		Devices:       []machine.BlockDevice{image, {Disk: "root2", Path: root2}},
		RecordedDisks: []machine.BlockDevice{data},
		Resources:     resources, RunDir: runs["m2"],
	}
	p2, err := d.DescribeRestore(ctx, "m2", spec2, held)
	require.NoError(t, err)
	assert.Equal(t, []machine.RunFile{{Name: diskdevice.Name(0, data.Disk), Path: data.Path}}, p2.RunFiles,
		"the recorded disk is a run file under its device's name")
	for position := 0; position <= 1; position++ {
		assert.NotEmpty(t, hostOf(p2.BootFiles, machine.InDisk(position)), "control: the Machine's own device %d is bound", position)
	}
	assert.Empty(t, hostOf(p2.BootFiles, machine.InDisk(2)), "the recorded disk is not bound at /disk/2")

	startLogged(t, s, p2.For(spec2), runs["m2"])
	require.NoError(t, d.Restore(ctx, "m2", spec2))
	require.Eventually(t, func() bool { return answers("m2") }, time.Minute, 500*time.Millisecond, "the Guest Agent never answered on the restored Machine")

	linked, err := os.Stat(filepath.Join(runs["m2"], diskdevice.Name(0, data.Disk)))
	require.NoError(t, err)
	file, err := os.Stat(data.Path)
	require.NoError(t, err)
	assert.True(t, os.SameFile(file, linked), "the link in the restored Machine's run directory is the disk's own file")
	assert.Equal(t, captured.Size(), file.Size(), "the disk's size is as the snapshot found it")
	assert.True(t, captured.ModTime().Equal(file.ModTime()), "the disk's modification time is as the snapshot found it: it was %v and is %v", captured.ModTime(), file.ModTime())

	for disk, want := range map[machine.DiskID]machine.DriveState{
		data.Disk: machine.DriveHotPlugged, root.Disk: machine.DriveAtBoot, "none": machine.DriveAbsent,
	} {
		got, err := d.Drive(ctx, "m2", disk)
		require.NoError(t, err, disk)
		assert.Equal(t, want, got, "the restored VMM's drive for %q", disk)
	}
	s.check(t, Restored, Machine{ID: "m2", RunDir: runs["m2"], Devices: spec2.Devices})
}

// bootPaths are the paths in the VMM's root of the boot files among files: the
// ones in /boot, where the kernel and the initrd are bound.
func bootPaths(files []machine.File) []string {
	var in []string
	for _, f := range files {
		if filepath.Dir(f.In) == "/boot" {
			in = append(in, f.In)
		}
	}
	return in
}

// hostOf is the host path bound at in, or "" for none.
func hostOf(files []machine.File, in string) string {
	for _, f := range files {
		if f.In == in {
			return f.Host
		}
	}
	return ""
}

// refusesSnapshot asserts the Driver refuses the snapshot operations as one that
// does not report the capability must: an error wrapping ErrUnsupported, which
// is the refusal for the capability and not for another cause such as a Machine
// that is not running.
func refusesSnapshot(ctx context.Context, t *testing.T, d machine.Driver, id machine.MachineID, spec machine.BootSpec) {
	t.Helper()
	_, err := d.Snapshot(ctx, id)
	assert.ErrorIs(t, err, machine.ErrUnsupported, "Snapshot")
	_, err = d.DescribeRestore(ctx, "restored", spec, nil)
	assert.ErrorIs(t, err, machine.ErrUnsupported, "DescribeRestore")
	assert.ErrorIs(t, d.Restore(ctx, "restored", spec), machine.ErrUnsupported, "Restore")
}
