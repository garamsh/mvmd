package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func mounted(disk machine.DiskID, at string) machine.DiskAttachment {
	return machine.DiskAttachment{Disk: disk, MountPath: at}
}

// badMountPaths are the mount paths a disk attachment refuses, each with what
// is wrong with it (ADR 0053, guest.md).
var badMountPaths = map[string]string{
	"not absolute":      "data",
	"not clean":         "/data/../x",
	"trailing slash":    "/data/",
	"the root":          "/",
	"under /proc":       "/proc/x",
	"under /sys":        "/sys/x",
	"under /dev":        "/dev/x",
	"/run/mvmd itself":  "/run/mvmd",
	"under /run/mvmd":   "/run/mvmd/x",
	"empty is no mount": "", // Control: it is the one that is accepted.
}

// ADR 0053 decision 2: CreateDisk takes none or ext4, and empty is ext4.
func TestCreateDisk_TakesAFilesystemAndTheListingReportsIt(t *testing.T) {
	ctx := context.Background()
	e := newEnv()

	requireKind(t, e.svc.CreateDisk(ctx, "bad", "r", 1<<20, "xfs"), machine.ErrInvalid)
	_, held := e.disks.Holds("bad")
	assert.False(t, held, "a refused call makes no disk")

	// Control: each filesystem that is named creates, so the refusal was the name.
	require.NoError(t, e.svc.CreateDisk(ctx, "raw", "r1", 1<<20, machine.FilesystemNone))
	require.NoError(t, e.svc.CreateDisk(ctx, "fs", "r2", 1<<20, machine.FilesystemExt4))
	require.NoError(t, e.svc.CreateDisk(ctx, "dflt", "r3", 1<<20, ""))

	listed, err := e.svc.ListDisks(ctx)
	require.NoError(t, err)
	got := map[machine.DiskID]machine.Filesystem{}
	for _, d := range listed {
		got[d.ID] = d.Filesystem
	}
	assert.Equal(t, map[machine.DiskID]machine.Filesystem{"raw": machine.FilesystemNone, "fs": machine.FilesystemExt4, "dflt": machine.FilesystemExt4}, got)

	// A repeat names the same filesystem, and another is a conflict.
	require.NoError(t, e.svc.CreateDisk(ctx, "raw", "r1", 1<<20, machine.FilesystemNone))
	requireKind(t, e.svc.CreateDisk(ctx, "raw", "r1", 1<<20, machine.FilesystemExt4), machine.ErrConflict)
	require.NoError(t, e.svc.CreateDisk(ctx, "dflt", "r3", 1<<20, machine.FilesystemExt4), "empty is ext4, so the repeat is the same call")
}

// ADR 0053 decision 5: a mount path on Attach disk is absolute, clean, not the
// root and not reserved, and the disk has a filesystem.
func TestAttachDisk_MountPathRules(t *testing.T) {
	ctx := context.Background()

	for name, p := range badMountPaths {
		t.Run(name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			e.created(t, "m")
			e.disks.Provide("d")

			err := e.svc.AttachDisk(ctx, "m", mounted("d", p))
			if p == "" {
				require.NoError(t, err)
				return
			}
			requireKind(t, err, machine.ErrInvalid)
			assert.Empty(t, attached(t, e, "m"))
			e.requireNoDiskHolder(t, "d")

			// Control: a good path attaches the same disk to the same Machine.
			require.NoError(t, e.svc.AttachDisk(ctx, "m", mounted("d", "/data")))
		})
	}

	t.Run("a disk with no filesystem", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.created(t, "m")
		require.NoError(t, e.svc.CreateDisk(ctx, "raw", "r1", 1<<20, machine.FilesystemNone))
		require.NoError(t, e.svc.CreateDisk(ctx, "fs", "r2", 1<<20, machine.FilesystemExt4))

		requireKind(t, e.svc.AttachDisk(ctx, "m", mounted("raw", "/data")), machine.ErrInvalid)
		assert.Empty(t, attached(t, e, "m"))
		e.requireNoDiskHolder(t, "raw")

		// Controls: the same disk attaches with no mount path, and a disk with
		// a filesystem attaches with one.
		require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("raw")))
		require.NoError(t, e.svc.AttachDisk(ctx, "m", mounted("fs", "/data")))
	})

	t.Run("a path another disk of the Machine has", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.created(t, "m")
		e.disks.Provide("a")
		e.disks.Provide("b")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", mounted("a", "/data")))

		requireKind(t, e.svc.AttachDisk(ctx, "m", mounted("b", "/data")), machine.ErrInvalid)
		assert.Equal(t, []machine.DiskAttachment{mounted("a", "/data")}, attached(t, e, "m"))

		require.NoError(t, e.svc.AttachDisk(ctx, "m", mounted("b", "/other")))
	})

	t.Run("a repeat with another mount path", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.created(t, "m")
		e.disks.Provide("d")
		require.NoError(t, e.svc.AttachDisk(ctx, "m", mounted("d", "/data")))

		requireKind(t, e.svc.AttachDisk(ctx, "m", mounted("d", "/other")), machine.ErrConflict)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", mounted("d", "/data")), "control: the same call repeats")
	})
}

// The same rules hold for the disks Create names.
func TestCreate_MountPathRules(t *testing.T) {
	ctx := context.Background()

	for name, p := range badMountPaths {
		t.Run(name, func(t *testing.T) {
			e := newEnv(machine.CapabilityDiskHotplug)
			e.disks.Provide("d")
			spec := baseSpec()
			spec.Disks = []machine.DiskAttachment{mounted("d", p)}

			err := e.svc.Create(ctx, "m", request("m"), spec)
			if p == "" {
				require.NoError(t, err)
				return
			}
			requireKind(t, err, machine.ErrInvalid)
			e.requireNoMachine(t, "m")
			e.requireNoDiskHolder(t, "d")

			spec.Disks = []machine.DiskAttachment{mounted("d", "/data")}
			require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec), "control: a good path creates")
		})
	}

	t.Run("a disk with no filesystem", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		require.NoError(t, e.svc.CreateDisk(ctx, "raw", "r1", 1<<20, machine.FilesystemNone))
		spec := baseSpec()
		spec.Disks = []machine.DiskAttachment{mounted("raw", "/data")}

		requireKind(t, e.svc.Create(ctx, "m", request("m"), spec), machine.ErrInvalid)
		e.requireNoMachine(t, "m")
		e.requireNoDiskHolder(t, "raw")

		spec.Disks = []machine.DiskAttachment{rw("raw")}
		require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec), "control: with no mount path it creates")
	})

	t.Run("two disks at one path", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("a")
		e.disks.Provide("b")
		spec := baseSpec()
		spec.Disks = []machine.DiskAttachment{mounted("a", "/data"), mounted("b", "/data")}

		requireKind(t, e.svc.Create(ctx, "m", request("m"), spec), machine.ErrInvalid)
		e.requireNoMachine(t, "m")

		spec.Disks = []machine.DiskAttachment{mounted("a", "/data"), mounted("b", "/more")}
		require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec), "control: two paths create")
	})
}

// The guest is told each disk's mount path and read-only flag when it finds the
// disk: at Start for a disk Create named, and at once for a hot attach; a
// Machine restored from a snapshot keeps it.
func TestMountPath_ReachesTheGuestWhenItFindsTheDisk(t *testing.T) {
	ctx := context.Background()

	t.Run("at Start", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.disks.Provide("a")
		e.disks.Provide("b")
		spec := baseSpec()
		spec.Disks = []machine.DiskAttachment{mounted("a", "/data"), {Disk: "b", ReadOnly: true, MountPath: "/ro"}}
		require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec))

		require.NoError(t, e.svc.Start(ctx, "m"))

		assert.Equal(t, []machine.DiskAttachment{mounted("a", "/data"), {Disk: "b", ReadOnly: true, MountPath: "/ro"}}, e.guest.Finds("m"))
	})

	t.Run("at a hot attach", func(t *testing.T) {
		e := newEnv(machine.CapabilityDiskHotplug)
		e.inState(t, "m", machine.StateRunning)
		e.disks.Provide("d")

		require.NoError(t, e.svc.AttachDisk(ctx, "m", mounted("d", "/data")))

		assert.Equal(t, []machine.DiskAttachment{mounted("d", "/data")}, e.guest.Finds("m"))
	})

	t.Run("a restore keeps it", func(t *testing.T) {
		e := newEnv(snapshotDiskCaps...)
		e.disks.Provide("d1")
		e.inState(t, "src", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "src", mounted("d1", "/data")))
		require.NoError(t, e.svc.Snapshot(ctx, "src", "snap", "r-snap"))
		require.NoError(t, e.svc.Stop(ctx, "src", time.Second))
		require.NoError(t, e.svc.Delete(ctx, "src"))

		all, err := e.svc.ListSnapshots(ctx)
		require.NoError(t, err)
		require.Len(t, all, 1)
		require.Len(t, all[0].Disks, 1)
		assert.Equal(t, "/data", all[0].Disks[0].MountPath, "the snapshot records the mount path")

		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))

		m := e.requireState(t, "new", machine.StateRunning)
		assert.Equal(t, []machine.DiskAttachment{mounted("d1", "/data")}, m.Disks)
	})
}
