package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// ADR 0055: a disk grows, and only grows. These tests hold a disk "d" of
// diskBytes, which a Machine "m" holds in the cases that need a holder.

const (
	diskBytes = 1 << 20
	grownTo   = 4 << 20
)

var (
	resizable   = []machine.Capability{machine.CapabilityDiskHotplug, machine.CapabilityDiskResize}
	hotplugOnly = []machine.Capability{machine.CapabilityDiskHotplug}
)

// withDisk makes the disk "d" and a Machine "m" that holds it as a, in the
// state. The guest of a Running Machine sees the device at its present size.
func (e *env) withDisk(t *testing.T, state machine.State, a machine.DiskAttachment) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{a}
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec))
	e.guest.SetDeviceSize("m", "d", diskBytes)
	switch state {
	case machine.StateCreated:
	case machine.StateRunning:
		require.NoError(t, e.svc.Start(ctx, "m"))
	case machine.StatePaused:
		require.NoError(t, e.svc.Start(ctx, "m"))
		require.NoError(t, e.svc.Pause(ctx, "m"))
	case machine.StateStopped:
		require.NoError(t, e.svc.Start(ctx, "m"))
		require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	case machine.StateFaulted:
		require.NoError(t, e.svc.Start(ctx, "m"))
		e.sup.FailAt("terminate", errBoom)
		require.ErrorIs(t, e.svc.Stop(ctx, "m", time.Second), machine.ErrFailed)
		e.sup.Heal("terminate")
	}
	e.requireState(t, "m", state)
}

func mountedAt(disk machine.DiskID) machine.DiskAttachment {
	return machine.DiskAttachment{Disk: disk, MountPath: "/data"}
}

func (e *env) requireDiskSize(t *testing.T, disk machine.DiskID, want uint64) {
	t.Helper()
	size, held := e.disks.Holds(disk)
	require.True(t, held, "disk %q", disk)
	assert.Equal(t, want, size, "the size of disk %q", disk)
}

// A size below the disk's is invalid and an equal size succeeds, and neither
// reaches the file; the control is a larger size, which does.
func TestResizeDisk_SmallerIsInvalidAndEqualChangesNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))

	requireKind(t, e.svc.ResizeDisk(ctx, "d", diskBytes-1), machine.ErrInvalid)
	e.requireDiskSize(t, "d", diskBytes)

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", diskBytes))
	e.requireDiskSize(t, "d", diskBytes)
	assert.Zero(t, e.disks.Calls("grow"), "neither was sent to the file")

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo))
	e.requireDiskSize(t, "d", grownTo)
	assert.Equal(t, 1, e.disks.Calls("grow"))
}

// The same rule holds for a disk a Machine holds, whatever the Machine is
// doing: a smaller size is invalid and an equal one is a success that sends
// nothing to the file, the Driver or the guest.
func TestResizeDisk_SmallerAndEqualOnAHeldDiskReachNothing(t *testing.T) {
	for _, state := range []machine.State{machine.StateCreated, machine.StateStopped, machine.StatePaused} {
		t.Run(state.String(), func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(resizable...)
			e.withDisk(t, state, mountedAt("d"))
			grows := e.guest.Calls("grow-disk")

			requireKind(t, e.svc.ResizeDisk(ctx, "d", diskBytes-1), machine.ErrInvalid)
			require.NoError(t, e.svc.ResizeDisk(ctx, "d", diskBytes))
			e.requireDiskSize(t, "d", diskBytes)
			assert.Zero(t, e.disks.Calls("grow"))
			assert.Zero(t, e.driver.Calls("resize-disk"))
			assert.Equal(t, grows, e.guest.Calls("grow-disk"))
		})
	}
}

// A disk the node does not hold is not found. The control is the same call
// once the node holds it.
func TestResizeDisk_AnUnknownDiskIsNotFound(t *testing.T) {
	ctx := context.Background()
	e := newEnv()

	requireKind(t, e.svc.ResizeDisk(ctx, "ghost", grownTo), machine.ErrNotFound)
	_, held := e.disks.Holds("ghost")
	assert.False(t, held, "a refused call makes no disk")

	e.disks.Provide("ghost")
	require.NoError(t, e.svc.ResizeDisk(ctx, "ghost", grownTo))
	e.requireDiskSize(t, "ghost", grownTo)
}

func TestResizeDisk_AnIdentifierNoDiskHasIsInvalid(t *testing.T) {
	e := newEnv()
	for _, id := range []machine.DiskID{"", "..", "a/b", "tmp-x"} {
		requireKind(t, e.svc.ResizeDisk(context.Background(), id, grownTo), machine.ErrInvalid)
	}
	assert.Zero(t, e.disks.Calls("get"), "the call never reached the port")
}

// A disk of a Created or Stopped Machine has its file grown and nothing else is
// asked: no VMM is running to be told, and the guest is not there to ask.
func TestResizeDisk_ADiskOfACreatedOrStoppedMachineGrowsOffline(t *testing.T) {
	for _, state := range []machine.State{machine.StateCreated, machine.StateStopped} {
		t.Run(state.String(), func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(hotplugOnly...)
			e.withDisk(t, state, mountedAt("d"))
			grows, drive := e.guest.Calls("grow-disk"), e.driver.Calls("resize-disk")

			require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo))

			e.requireDiskSize(t, "d", grownTo)
			assert.Equal(t, drive, e.driver.Calls("resize-disk"), "the Driver was not asked")
			assert.Equal(t, grows, e.guest.Calls("grow-disk"), "the guest was not asked")
			e.requireState(t, "m", state)
		})
	}
}

// A disk that no Machine holds is listed with no holder after the call.
func TestResizeDisk_ADiskNoMachineHoldsGrowsAndStaysUnheld(t *testing.T) {
	ctx := context.Background()
	e := newEnv()
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemNone))

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo))

	listed, err := e.svc.ListDisks(ctx)
	require.NoError(t, err)
	require.Len(t, listed, 1)
	assert.Equal(t, machine.DiskInfo{ID: "d", SizeBytes: grownTo, Filesystem: machine.FilesystemNone}, listed[0])
	require.NoError(t, e.svc.DeleteDisk(ctx, "d"), "control: the holder slot was given back")
}

// While a disk no Machine holds is being grown, no attach takes it and no delete
// removes it, and the listing does not name a holder. Once it is done, both can.
func TestResizeDisk_ADiskBeingGrownIsHeldAgainstAnAttachAndADelete(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugOnly...)
	require.NoError(t, e.svc.CreateDisk(ctx, "d", "r-d", diskBytes, machine.FilesystemExt4))
	e.created(t, "m")
	entered, release := e.disks.Hold("grow")

	done := make(chan error, 1)
	go func() { done <- e.svc.ResizeDisk(ctx, "d", grownTo) }()
	<-entered

	requireKind(t, e.svc.AttachDisk(ctx, "m", rw("d")), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.DeleteDisk(ctx, "d"), machine.ErrPreconditionFailed)
	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo+diskBytes), machine.ErrPreconditionFailed)
	listed, err := e.svc.ListDisks(ctx)
	require.NoError(t, err)
	assert.Empty(t, listed[0].Holder)

	release()
	require.NoError(t, <-done)
	require.NoError(t, e.svc.AttachDisk(ctx, "m", rw("d")), "control: once the growth is done the disk attaches")
}

// A disk a Running Machine holds, on a Driver that reports the capability: the
// Driver grows the drive, the guest is asked until it sees the new size and has
// grown the filesystem, and the call returns after that.
func TestResizeDisk_ARunningMachinesDiskGrowsLiveOnADriverThatCan(t *testing.T) {
	ctx := context.Background()
	e := newEnv(resizable...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	e.guest.SetDeviceSize("m", "d", grownTo)
	before := e.guest.Calls("grow-disk")

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo))

	assert.Equal(t, uint64(grownTo), e.driver.DriveSize("m", "d"), "the Driver grew the drive")
	assert.Greater(t, e.guest.Calls("grow-disk"), before, "the guest was asked to grow the filesystem")
	assert.Zero(t, e.disks.Calls("grow"), "the host file is the VMM's to grow, not the node's")
	e.requireState(t, "m", machine.StateRunning)
}

// The guest may see the new size after the VMM has taken it; the call asks again
// until it does.
func TestResizeDisk_ALiveGrowthWaitsForTheGuestToSeeTheSize(t *testing.T) {
	ctx := context.Background()
	e := newEnv(resizable...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	before := e.guest.Calls("grow-disk")
	go func() {
		time.Sleep(200 * time.Millisecond)
		e.guest.SetDeviceSize("m", "d", grownTo)
	}()

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo))

	assert.Greater(t, e.guest.Calls("grow-disk"), before+1, "asked again until it saw the size")
}

// A guest that never sees the size fails the call; the Driver has already grown
// the drive and the file, so the repeat, which is then an equal size on a live
// disk, asks the guest again and converges.
func TestResizeDisk_AGuestThatNeverSeesTheSizeFailsTheCallAndARepeatConverges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(resizable...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	e.svc = e.reopenedWith(t, machine.WithResizeTimeout(300*time.Millisecond))

	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrFailed)
	assert.Equal(t, uint64(grownTo), e.driver.DriveSize("m", "d"))

	// The VMM has set the length of the file by now, and the guest sees it.
	e.disks.Resize("d", grownTo)
	e.guest.SetDeviceSize("m", "d", grownTo)
	calls := e.driver.Calls("resize-disk")
	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo), "the same size, asked again, finishes the growth")
	assert.Greater(t, e.driver.Calls("resize-disk"), calls)
}

func TestResizeDisk_AGuestFailureFailsALiveGrowthAndADriverFailureAsksTheGuestNothing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(resizable...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	e.guest.SetDeviceSize("m", "d", grownTo)

	e.driver.FailAt("resize-disk", errBoom)
	before := e.guest.Calls("grow-disk")
	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrFailed)
	assert.Equal(t, before, e.guest.Calls("grow-disk"), "the VMM has no new size to show the guest")
	e.driver.Heal("resize-disk")

	e.guest.FailAt("grow-disk", errBoom)
	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrFailed)
	e.guest.Heal("grow-disk")

	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo), "control: healed, the same call grows")
}

// A Running Machine's disk on a Driver without the capability is refused, and
// nothing is changed or asked. The controls: the same call on a Driver with it,
// and the same call once the Machine is Stopped.
func TestResizeDisk_ARunningMachinesDiskOnADriverWithoutTheCapabilityIsRefused(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugOnly...)
	e.withDisk(t, machine.StateRunning, mountedAt("d"))
	before := e.guest.Calls("grow-disk")

	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrPreconditionFailed)

	e.requireDiskSize(t, "d", diskBytes)
	assert.Zero(t, e.driver.Calls("resize-disk"), "the Driver was not asked")
	assert.Zero(t, e.disks.Calls("grow"))
	assert.Equal(t, before, e.guest.Calls("grow-disk"))

	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo), "control: Stopped, the same disk grows")
	e.requireDiskSize(t, "d", grownTo)
}

// A Paused Machine's disk is refused whatever the Driver reports, and a Faulted
// Machine's too. The control is the same disk after the Machine is Running again.
func TestResizeDisk_APausedOrFaultedMachinesDiskIsRefused(t *testing.T) {
	for _, state := range []machine.State{machine.StatePaused, machine.StateFaulted} {
		t.Run(state.String(), func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(resizable...)
			e.withDisk(t, state, mountedAt("d"))

			requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrPreconditionFailed)

			e.requireDiskSize(t, "d", diskBytes)
			assert.Zero(t, e.driver.Calls("resize-disk"))
			assert.Zero(t, e.disks.Calls("grow"))

			if state == machine.StatePaused {
				require.NoError(t, e.svc.Resume(ctx, "m"))
				e.guest.SetDeviceSize("m", "d", grownTo)
				require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo), "control: Running, the same call grows")
			}
		})
	}
}

// A Machine's root disk is held for its life and does not grow, in any state.
// The control is a disk attached to the same Machine.
func TestResizeDisk_ARootDiskIsRefused(t *testing.T) {
	for _, state := range []machine.State{machine.StateCreated, machine.StateStopped, machine.StateRunning} {
		t.Run(state.String(), func(t *testing.T) {
			ctx := context.Background()
			e := newEnv(resizable...)
			e.withDisk(t, state, mountedAt("d"))
			root := machine.RootDiskID("m")
			rootBytes, _ := e.disks.Holds(root)

			requireKind(t, e.svc.ResizeDisk(ctx, root, rootBytes+diskBytes), machine.ErrPreconditionFailed)

			e.requireDiskSize(t, root, rootBytes)
			assert.Zero(t, e.driver.Calls("resize-disk"))
			assert.Zero(t, e.disks.Calls("grow"))
		})
	}
}

// A disk attached read-only has a drive the VMM opened read-only, which it
// cannot grow, so a live growth is refused; offline it grows.
func TestResizeDisk_AReadOnlyDiskGrowsOnlyOffline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(resizable...)
	e.withDisk(t, machine.StateRunning, machine.DiskAttachment{Disk: "d", MountPath: "/data", ReadOnly: true})

	requireKind(t, e.svc.ResizeDisk(ctx, "d", grownTo), machine.ErrPreconditionFailed)
	assert.Zero(t, e.driver.Calls("resize-disk"))

	require.NoError(t, e.svc.Stop(ctx, "m", time.Second))
	require.NoError(t, e.svc.ResizeDisk(ctx, "d", grownTo), "control: Stopped, the same disk grows")
}

// The file is grown while the Machine is stopped; the next start has the guest
// grow the filesystem of each mounted writable disk, after the guest has mounted
// them. A disk with no mount and one mounted read-only have no filesystem to
// grow, so the guest is not asked of them.
func TestStart_TheGuestGrowsTheFilesystemOfEachWritableMountedDisk(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugOnly...)
	for _, d := range []machine.DiskID{"w", "n", "r"} {
		require.NoError(t, e.svc.CreateDisk(ctx, d, machine.RequestID("r-"+d), diskBytes, machine.FilesystemExt4))
	}
	spec := baseSpec()
	spec.Disks = []machine.DiskAttachment{
		{Disk: "w", MountPath: "/w"},
		{Disk: "n"},
		{Disk: "r", MountPath: "/r", ReadOnly: true},
	}
	require.NoError(t, e.svc.Create(ctx, "m", request("m"), spec))
	require.NoError(t, e.svc.ResizeDisk(ctx, "w", grownTo))

	require.NoError(t, e.svc.Start(ctx, "m"))

	assert.Equal(t, []machine.DiskID{"w"}, e.guest.Grows("m"))
	assert.Len(t, e.guest.Boots("m"), 1)
}

// A guest that cannot grow the filesystem fails the start, so a Machine is not
// Running with a disk smaller than its device. Healed, the same start succeeds.
func TestStart_AGuestThatCannotGrowAFilesystemFailsTheStart(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugOnly...)
	e.withDisk(t, machine.StateCreated, mountedAt("d"))
	e.guest.FailAt("grow-disk", errBoom)

	requireKind(t, e.svc.Start(ctx, "m"), machine.ErrFailed)
	e.requireState(t, "m", machine.StateStopped)

	e.guest.Heal("grow-disk")
	require.NoError(t, e.svc.Start(ctx, "m"), "control: healed, the same start succeeds")
}
