package machine_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// osEnv is an env whose node offers the memory Driver's boot set "os".
func osEnv(t *testing.T, caps ...machine.Capability) (*env, *memory.BootFiles) {
	t.Helper()
	e := newEnv(caps...)
	files := &memory.BootFiles{}
	e.svc = e.reopenedWith(t, machine.WithBootSets(memory.BootSets(), files))
	return e, files
}

func osSpec() machine.Spec {
	spec := baseSpec()
	spec.Image, spec.RootDiskBytes = "", 0
	spec.OS = &machine.OSBoot{BootSet: "os", CommandLine: "console=ttyS0 quiet"}
	return spec
}

func (e *env) osCreated(t *testing.T, id machine.MachineID, disks ...machine.DiskAttachment) {
	t.Helper()
	spec := osSpec()
	spec.Disks = disks
	require.NoError(t, e.svc.Create(context.Background(), id, request(id), spec))
	e.requireState(t, id, machine.StateCreated)
}

func TestCreate_OSForm(t *testing.T) {
	ctx := context.Background()

	t.Run("an OS-form Machine is created with no image and no root disk", func(t *testing.T) {
		e, _ := osEnv(t)
		e.osCreated(t, "m")
		m := e.requireState(t, "m", machine.StateCreated)
		require.NotNil(t, m.OS)
		assert.Equal(t, "os", m.OS.BootSet)
		assert.Empty(t, m.Image)
		assert.Empty(t, m.RootDisk)
	})

	for _, tc := range []struct {
		field string
		set   func(*machine.Spec)
	}{
		{"image_digest", func(s *machine.Spec) { s.Image = image }},
		{"root_disk_bytes", func(s *machine.Spec) { s.RootDiskBytes = 1 << 30 }},
		{"kernel", func(s *machine.Spec) { s.Kernel = "stable" }},
		{"command", func(s *machine.Spec) { s.Command = []string{"/bin/true"} }},
		{"args", func(s *machine.Spec) { s.Args = []string{"-x"} }},
		{"env", func(s *machine.Spec) { s.Env = []machine.EnvVar{{Name: "A", Value: "b"}} }},
		{"user", func(s *machine.Spec) { s.User = "1000" }},
		{"working_dir", func(s *machine.Spec) { s.WorkingDir = "/srv" }},
		{"files", func(s *machine.Spec) {
			s.Files = []machine.PlacedFile{{Path: "/etc/x", Content: []byte("x"), Mode: 0o644}}
		}},
		{"probes", func(s *machine.Spec) { s.Probes = []machine.Probe{execProbe} }},
	} {
		t.Run("refuses "+tc.field+" and names it", func(t *testing.T) {
			e, _ := osEnv(t)
			spec := osSpec()
			tc.set(&spec)
			err := e.svc.Create(ctx, "m", request("m"), spec)
			requireKind(t, err, machine.ErrInvalid)
			assert.ErrorContains(t, err, tc.field)
			_, held := e.store.Record("m")
			assert.False(t, held)
			require.NoError(t, e.svc.Create(ctx, "m", request("m"), osSpec()), "control: the OS form alone is accepted")
		})
	}

	t.Run("refuses a disk mount path", func(t *testing.T) {
		e, _ := osEnv(t)
		e.disks.Provide("d")
		spec := osSpec()
		spec.Disks = []machine.DiskAttachment{{Disk: "d", MountPath: "/data"}}
		requireKind(t, e.svc.Create(ctx, "m", request("m"), spec), machine.ErrInvalid)
		spec.Disks[0].MountPath = ""
		require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec), "control: the disk without a mount path")
	})

	t.Run("refuses a boot set the node does not offer", func(t *testing.T) {
		e, _ := osEnv(t)
		spec := osSpec()
		spec.OS.BootSet = "nope"
		requireKind(t, e.svc.Create(ctx, "m", request("m"), spec), machine.ErrInvalid)
		require.NoError(t, e.svc.Create(ctx, "m", request("m"), osSpec()), "control: an offered name")
	})
}

func TestStart_OSForm(t *testing.T) {
	ctx := context.Background()

	t.Run("reaches Running with the boot set, command line and disks in order, and no agent call", func(t *testing.T) {
		e, _ := osEnv(t)
		e.disks.Provide("a")
		e.disks.Provide("b")
		e.osCreated(t, "m", machine.DiskAttachment{Disk: "b"}, machine.DiskAttachment{Disk: "a", ReadOnly: true})
		require.NoError(t, e.svc.Start(ctx, "m"))
		e.requireState(t, "m", machine.StateRunning)

		boot := e.driver.BootedWith("m")
		set := memory.BootSets()["os"]
		assert.Equal(t, set.Kernel, boot.Kernel)
		assert.Equal(t, set.Initrd, boot.Initrd)
		assert.Equal(t, "console=ttyS0 quiet", boot.CommandLine)
		require.Len(t, boot.Devices, 2)
		assert.Equal(t, machine.DiskID("b"), boot.Devices[0].Disk)
		assert.Equal(t, machine.DiskID("a"), boot.Devices[1].Disk)
		assert.True(t, boot.Devices[1].ReadOnly)
		assert.Empty(t, e.guest.Boots("m"), "the guest was not told to boot")
		assert.Zero(t, e.guest.Calls("health"))
	})

	t.Run("a changed file fails Start as a failed precondition naming it", func(t *testing.T) {
		for _, tc := range []struct{ what, path string }{
			{"kernel", memory.BootSets()["os"].Kernel.Path},
			{"initrd", memory.BootSets()["os"].Initrd.Path},
		} {
			t.Run(tc.what, func(t *testing.T) {
				e, files := osEnv(t)
				e.osCreated(t, "m")
				require.NoError(t, e.svc.Start(ctx, "m"), "control: unchanged files start")
				require.NoError(t, e.svc.ForceStop(ctx, "m"))
				files.Change(tc.path)
				err := e.svc.Start(ctx, "m")
				requireKind(t, err, machine.ErrPreconditionFailed)
				assert.ErrorContains(t, err, "the "+tc.what+" of boot set")
				e.requireState(t, "m", machine.StateStopped)
			})
		}
	})
}

func TestStop_OSFormCallsNoAgent(t *testing.T) {
	ctx := context.Background()
	e, _ := osEnv(t)
	e.osCreated(t, "m")
	require.NoError(t, e.svc.Start(ctx, "m"))
	e.guest.EndsMachineWith(func(machine.MachineID) { t.Error("the agent was asked to end the Machine") })
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	e.requireState(t, "m", machine.StateStopped)
	assert.Zero(t, e.guest.Calls("terminate"))

	// Control: an image-form Machine's graceful Stop does ask the agent.
	e.guest.EndsMachineWith(func(id machine.MachineID) { _ = e.driver.Shutdown(ctx, id, 0) })
	e.created(t, "img")
	require.NoError(t, e.svc.Start(ctx, "img"))
	require.NoError(t, e.svc.Stop(ctx, "img", time.Second))
	assert.Positive(t, e.guest.Calls("terminate"))
}

func TestAttachDetach_OSFormOnlyWhileCreatedOrStopped(t *testing.T) {
	ctx := context.Background()
	e, _ := osEnv(t)
	e.disks.Provide("a")
	e.disks.Provide("b")
	e.osCreated(t, "m")

	require.NoError(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "a"}), "Created")
	require.NoError(t, e.svc.DetachDisk(ctx, "m", "a"), "Created")
	require.NoError(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "a"}))
	require.NoError(t, e.svc.Start(ctx, "m"))

	requireKind(t, e.svc.DetachDisk(ctx, "m", "a"), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "b"}), machine.ErrPreconditionFailed)
	require.NoError(t, e.svc.Pause(ctx, "m"))
	requireKind(t, e.svc.DetachDisk(ctx, "m", "a"), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "b"}), machine.ErrPreconditionFailed)
	require.NoError(t, e.svc.Resume(ctx, "m"))

	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "b"}), "Stopped")
	require.NoError(t, e.svc.DetachDisk(ctx, "m", "a"), "Stopped")
}

func TestGuestCalls_OSFormIsAFailedPrecondition(t *testing.T) {
	ctx := context.Background()
	e, _ := osEnv(t, machine.CapabilitySnapshot)
	e.osCreated(t, "m")
	require.NoError(t, e.svc.Start(ctx, "m"))
	e.created(t, "img")
	require.NoError(t, e.svc.Start(ctx, "img"))
	e.guest.PutFile("img", "/f", "x")

	read := func(id machine.MachineID) error {
		return e.svc.ReadFile(ctx, id, "/f", func([]byte) error { return nil })
	}
	exec := func(id machine.MachineID) error {
		_, err := e.svc.Exec(ctx, id, "x1", "req-x1-"+machine.RequestID(id), machine.ExecSpec{Command: []string{"true"}})
		return err
	}
	snap := func(id machine.MachineID) error {
		return e.svc.Snapshot(ctx, id, "s-"+machine.SnapshotID(id), "rs-"+machine.RequestID(id))
	}

	for name, call := range map[string]func(machine.MachineID) error{"exec": exec, "read file": read} {
		t.Run(name, func(t *testing.T) {
			err := call("m")
			requireKind(t, err, machine.ErrPreconditionFailed)
			assert.ErrorContains(t, err, "OS-form")
			require.NoError(t, call("img"), "control: the image form")
		})
	}
	t.Run("snapshot", func(t *testing.T) {
		err := snap("m")
		requireKind(t, err, machine.ErrPreconditionFailed)
		assert.ErrorContains(t, err, "OS-form")
		require.NoError(t, snap("img"), "control: the image form")
	})
	t.Run("read exec output of the entrypoint and signal exec", func(t *testing.T) {
		_, err := e.guest.Exec(ctx, "img", "entrypoint", machine.ExecSpec{Command: []string{"true"}})
		require.NoError(t, err)
		readEntrypoint := func(id machine.MachineID) error {
			_, err := e.svc.ReadExecOutput(ctx, id, "entrypoint", 0, 0, func(machine.OutputChunk) error { return nil })
			return err
		}
		signal := func(id machine.MachineID) error { return e.svc.SignalExec(ctx, id, "entrypoint", 15) }

		for name, call := range map[string]func(machine.MachineID) error{"read exec output": readEntrypoint, "signal exec": signal} {
			err := call("m")
			requireKind(t, err, machine.ErrPreconditionFailed)
			assert.ErrorContains(t, err, "OS-form", name)
			require.NoError(t, call("img"), "control: the image form, %s", name)
		}

		require.NoError(t, e.svc.ForceStop(ctx, "m"))
		err = readEntrypoint("m")
		requireKind(t, err, machine.ErrPreconditionFailed)
		assert.ErrorContains(t, err, "OS-form", "Stopped")
	})
}

func TestResizeDisk_OSFormOnlyWhileCreatedOrStopped(t *testing.T) {
	ctx := context.Background()
	e, _ := osEnv(t, resizable...)
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	e.osCreated(t, "m", machine.DiskAttachment{Disk: "d"})
	e.guest.SetDeviceSize("m", "d", diskBytes)
	grown := uint64(diskBytes * 2)

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grown), "control: Created")
	require.NoError(t, e.svc.Start(ctx, "m"))
	grown += uint64(diskBytes)
	err := e.svc.ResizeDisk(ctx, "d", grown)
	requireKind(t, err, machine.ErrPreconditionFailed)
	assert.ErrorContains(t, err, "OS-form")
	assert.Zero(t, e.driver.DriveSize("m", "d"), "the Driver was not asked to grow the drive")
	assert.Zero(t, e.guest.Calls("grow-disk"), "the guest was not asked")
	require.NoError(t, e.svc.Pause(ctx, "m"))
	requireKind(t, e.svc.ResizeDisk(ctx, "d", grown), machine.ErrPreconditionFailed)
	require.NoError(t, e.svc.Resume(ctx, "m"))
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grown), "control: Stopped")

	// Control: the image form's Running Machine grows the same disk live.
	img := newEnv(resizable...)
	img.withDisk(t, machine.StateRunning, machine.DiskAttachment{Disk: "d"})
	img.guest.SetDeviceSize("m", "d", grownTo)
	require.NoError(t, img.svc.ResizeDisk(ctx, "d", grownTo))
	assert.Equal(t, uint64(grownTo), img.driver.DriveSize("m", "d"))
}

func TestSetResources_OSFormRefusesALiveChange(t *testing.T) {
	ctx := context.Background()
	more := machine.Resources{VCPUs: 3, MemoryBytes: 512 << 20}
	created := func(t *testing.T) *env {
		e, _ := osEnv(t, hotplugAndResize...)
		spec := osSpec()
		spec.Ceiling = large
		require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec))
		return e
	}
	for _, state := range []machine.State{machine.StateRunning, machine.StatePaused} {
		t.Run(state.String(), func(t *testing.T) {
			e := created(t)
			require.NoError(t, e.svc.Start(ctx, "m"))
			if state == machine.StatePaused {
				require.NoError(t, e.svc.Pause(ctx, "m"))
			}
			require.NoError(t, e.svc.SetResources(ctx, "m", small, machine.Resources{}), "control: a repeat of the held figures")
			requireKind(t, e.svc.SetResources(ctx, "m", more, machine.Resources{}), machine.ErrPreconditionFailed)
			assert.Equal(t, small, e.requireState(t, "m", state).Resources)
		})
	}
	t.Run("Running names the OS form", func(t *testing.T) {
		e := created(t)
		require.NoError(t, e.svc.Start(ctx, "m"))
		assert.ErrorContains(t, e.svc.SetResources(ctx, "m", more, machine.Resources{}), "OS-form")
	})
	t.Run("controls", func(t *testing.T) {
		require.NoError(t, created(t).svc.SetResources(ctx, "m", more, machine.Resources{}), "Created accepts the change")

		img := newEnv(hotplugAndResize...)
		img.ceiling = large
		img.inState(t, "m", machine.StateRunning)
		require.NoError(t, img.svc.SetResources(ctx, "m", more, machine.Resources{}), "the image form's Running Machine")
	})
}

func TestStart_OSFormReportsADriverThatCannotBootItAsUnsupported(t *testing.T) {
	ctx := context.Background()
	e, _ := osEnv(t)
	e.osCreated(t, "m")
	e.driver.FailAt("describe", fmt.Errorf("no initrd: %w", machine.ErrUnsupported))
	requireKind(t, e.svc.Start(ctx, "m"), machine.ErrUnsupported)
	e.requireState(t, "m", machine.StateCreated)

	e.driver.FailAt("describe", errBoom)
	requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
	e.requireState(t, "m", machine.StateCreated)

	e.driver.Heal("describe")
	require.NoError(t, e.svc.Start(ctx, "m"), "control: a Driver that can")
}

func TestStart_OSFormRefusesARunUpdate(t *testing.T) {
	ctx := context.Background()
	e, _ := osEnv(t)
	e.osCreated(t, "m")
	requireKind(t, e.svc.StartWith(ctx, "m", newRun()), machine.ErrInvalid)
	e.requireState(t, "m", machine.StateCreated)
	require.NoError(t, e.svc.Start(ctx, "m"), "control: a Start with no update")
	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	requireKind(t, e.svc.StartWith(ctx, "m", newRun()), machine.ErrInvalid)
	e.requireState(t, "m", machine.StateStopped)
	require.NoError(t, e.svc.Start(ctx, "m"), "control: Stopped, no update")
}
