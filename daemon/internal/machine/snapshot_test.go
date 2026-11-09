package machine_test

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

var snapshotCaps = []machine.Capability{machine.CapabilitySnapshot}

// snapshotted takes a Machine to the state and snapshots it as "snap" under
// the request "r-snap".
func (e *env) snapshotted(t *testing.T, id machine.MachineID, state machine.State) {
	t.Helper()
	e.inState(t, id, state)
	require.NoError(t, e.svc.Snapshot(context.Background(), id, "snap", "r-snap"))
	require.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held(), "the setup: the snapshot exists")
}

// elsewhere is a node of its own that holds the same snapshots and is
// described by info. It holds no image yet.
func (e *env) elsewhere(info machine.DriverInfo) *env {
	o := &env{
		driver: memory.NewDriver(info),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		snaps:  e.snaps,
		guest:  memory.NewGuest(0),
		clock:  memory.NewWallClock(epoch),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
		events: memory.NewEvents(),
	}
	o.svc = machine.NewService(o.driver, o.sup, o.net, o.disks, o.images, o.snaps, o.guest, o.clock, o.store, o.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), o.events)
	return o
}

var narrowPolicy = machine.NetworkPolicy{Egress: []machine.NetworkRule{{
	Prefix:   netip.MustParsePrefix("192.168.0.0/16"),
	Protocol: machine.ProtocolTCP,
	Ports:    []machine.PortRange{{First: 22, Last: 22}},
}}}

// Row "Snapshot", column Needs.
func TestSnapshot_Needs(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		from machine.State
		kind error
	}{
		{machine.StateRunning, nil},
		{machine.StatePaused, nil},
		{machine.StateCreated, machine.ErrPreconditionFailed},
		{machine.StateStopped, machine.ErrPreconditionFailed},
		{machine.StateFaulted, machine.ErrPreconditionFailed},
	} {
		t.Run(tt.from.String(), func(t *testing.T) {
			e := newEnv(snapshotCaps...)
			e.inState(t, "m", tt.from)
			err := e.svc.Snapshot(ctx, "m", "snap", "r")
			if tt.kind == nil {
				require.NoError(t, err)
				assert.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held())
				return
			}
			requireKind(t, err, tt.kind)
			assert.Empty(t, e.snaps.Held())
			e.requireState(t, "m", tt.from)
		})
	}

	t.Run("an absent Machine", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		requireKind(t, e.svc.Snapshot(ctx, "nobody", "snap", "r"), machine.ErrNotFound)
	})
	t.Run("a Driver that does not support snapshot", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		e.inState(t, "m", machine.StateRunning)
		requireKind(t, e.svc.Snapshot(ctx, "m", "snap", "r"), machine.ErrUnsupported)
		assert.Empty(t, e.snaps.Held())
		e.requireState(t, "m", machine.StateRunning)
	})
	t.Run("no disk attached but the root", func(t *testing.T) {
		e := newEnv(machine.CapabilitySnapshot, machine.CapabilityDiskHotplug)
		e.disks.Provide("data")
		e.inState(t, "m", machine.StateRunning)
		require.NoError(t, e.svc.AttachDisk(ctx, "m", machine.DiskAttachment{Disk: "data"}))

		requireKind(t, e.svc.Snapshot(ctx, "m", "snap", "r"), machine.ErrPreconditionFailed)
		assert.Empty(t, e.snaps.Held())
		assert.Zero(t, e.driver.Calls("snapshot"), "the guest was not paused for it")
		e.requireState(t, "m", machine.StateRunning)

		// Control: the same Machine, once the disk is detached.
		require.NoError(t, e.svc.DetachDisk(ctx, "m", "data"))
		require.NoError(t, e.svc.Snapshot(ctx, "m", "snap", "r"))
		assert.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held())
	})
	t.Run("an identifier or request ID that is empty", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "m", machine.StateRunning)
		requireKind(t, e.svc.Snapshot(ctx, "m", "", "r"), machine.ErrInvalid)
		requireKind(t, e.svc.Snapshot(ctx, "m", "snap", ""), machine.ErrInvalid)
		assert.Empty(t, e.snaps.Held())
	})
}

// Row "Snapshot", column On success.
func TestSnapshot_OnSuccess(t *testing.T) {
	ctx := context.Background()

	for _, from := range []machine.State{machine.StateRunning, machine.StatePaused} {
		t.Run("the Machine is left "+from.String(), func(t *testing.T) {
			e := newEnv(snapshotCaps...)
			e.snapshotted(t, "m", from)

			e.requireState(t, "m", from)
			assert.True(t, e.driver.PausedAtSnapshot("m"), "the guest was paused while it was taken")
			assert.Equal(t, from == machine.StatePaused, e.driver.Paused("m"), "and is as it was after")
		})
	}

	t.Run("the compatibility metadata and the files are recorded", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "m", machine.StateRunning)
		m := e.requireState(t, "m", machine.StateRunning)

		got, files, err := e.snaps.Get(ctx, "snap")
		require.NoError(t, err)
		assert.Equal(t, machine.SnapshotCompat{Driver: "inmem", Version: "1", CPUModel: "cpu-a", KernelDigest: memory.KernelDigest}, got.Compat)
		assert.Equal(t, machine.MachineID("m"), got.Machine)
		assert.Equal(t, m.Network.MAC, got.MAC, "the MAC the guest uses")
		assert.Equal(t, machine.Spec{Image: image, Resources: m.Resources, RootDiskBytes: m.RootDiskBytes}, got.Spec,
			"a snapshot carries no policy and no attached disk")
		assert.Equal(t, "/disks/m-root", files.RootDisk, "the root disk goes with the memory")
		assert.Equal(t, e.driver.BootSpec("m").RunDir+"/snapshot.mem", files.State[0], "the Driver's files in the run directory")
	})
}

// Row "Snapshot", column Repeat.
func TestSnapshot_Repeat(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "m", machine.StateRunning)

	require.NoError(t, e.svc.Snapshot(ctx, "m", "snap", "r-snap"), "the same request ID: the existing snapshot")
	assert.Equal(t, 1, e.driver.Calls("snapshot"), "the guest is not paused and taken a second time")
	assert.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held())

	requireKind(t, e.svc.Snapshot(ctx, "m", "snap", "another"), machine.ErrConflict)
	e.inState(t, "other", machine.StateRunning)
	requireKind(t, e.svc.Snapshot(ctx, "other", "snap", "r-snap"), machine.ErrConflict)

	// Control: a snapshot of its own is made.
	require.NoError(t, e.svc.Snapshot(ctx, "other", "snap-2", "r-2"))
	assert.Equal(t, []machine.SnapshotID{"snap", "snap-2"}, e.snaps.Held())
}

// Row "Snapshot", column State after failure.
func TestSnapshot_StateAfterFailure(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		from machine.State
		fail func(*env)
		heal func(*env)
	}{
		{"the pause fails", machine.StateRunning, func(e *env) { e.driver.FailAt("pause", errBoom) }, func(e *env) { e.driver.Heal("pause") }},
		{"the Driver cannot take it", machine.StateRunning, func(e *env) { e.driver.FailAt("snapshot", errBoom) }, func(e *env) { e.driver.Heal("snapshot") }},
		{"the Driver cannot take it of a Paused Machine", machine.StatePaused, func(e *env) { e.driver.FailAt("snapshot", errBoom) }, func(e *env) { e.driver.Heal("snapshot") }},
		{"the store cannot keep it", machine.StateRunning, func(e *env) { e.snaps.FailAt("put", errBoom) }, func(e *env) { e.snaps.Heal("put") }},
		{"the root disk cannot be found", machine.StateRunning, func(e *env) { e.disks.FailAt("locate", errBoom) }, func(e *env) { e.disks.Heal("locate") }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(snapshotCaps...)
			e.inState(t, "m", tt.from)
			tt.fail(e)

			requireKind(t, e.svc.Snapshot(ctx, "m", "snap", "r"), machine.ErrFailed)
			e.requireState(t, "m", tt.from)
			assert.Empty(t, e.snaps.Held(), "no snapshot exists")
			assert.Equal(t, tt.from == machine.StatePaused, e.driver.Paused("m"), "the guest is as it was")

			// Control: the same call succeeds once the step stops failing.
			tt.heal(e)
			require.NoError(t, e.svc.Snapshot(ctx, "m", "snap", "r"))
			assert.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held())
		})
	}

	t.Run("the Driver's files of a failed attempt are named as the next attempt's", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "m", machine.StateRunning)
		e.snaps.FailAt("put", errBoom)
		requireKind(t, e.svc.Snapshot(ctx, "m", "snap", "r"), machine.ErrFailed)
		e.snaps.Heal("put")
		require.NoError(t, e.svc.Snapshot(ctx, "m", "snap", "r"))

		offered := e.snaps.Offered()
		require.Len(t, offered, 2)
		assert.Equal(t, offered[0], offered[1], "the next attempt writes over what the failed one left")
	})
	t.Run("a Machine that cannot be returned to Running is Faulted and leaves no snapshot", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "m", machine.StateRunning)
		e.driver.FailAt("resume", errBoom)

		requireKind(t, e.svc.Snapshot(ctx, "m", "snap", "r"), machine.ErrFailed)
		m := e.requireState(t, "m", machine.StateFaulted)
		assert.Contains(t, m.FailureReason, "returned to Running")
		assert.Empty(t, e.snaps.Held())
		e.requireStored(t, "m", machine.StateFaulted)
	})
	t.Run("and a snapshot that cannot be removed is said to remain", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "m", machine.StateRunning)
		e.driver.FailAt("resume", errBoom)
		e.snaps.FailAt("remove", errBoom)

		err := e.svc.Snapshot(ctx, "m", "snap", "r")
		requireKind(t, err, machine.ErrFailed)
		assert.Contains(t, err.Error(), "could not be removed")
		e.requireState(t, "m", machine.StateFaulted)
	})
	t.Run("control: a Paused Machine is never resumed, so it is never Faulted by it", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "m", machine.StatePaused)
		e.driver.FailAt("resume", errBoom)

		require.NoError(t, e.svc.Snapshot(ctx, "m", "snap", "r"))
		e.requireState(t, "m", machine.StatePaused)
	})
}

// Row "Restore", column Needs: a snapshot compatible with this node's Driver,
// version and CPU.
func TestRestoreSnapshot_NeedsACompatibleSnapshot(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	same := machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: snapshotCaps}

	for _, tt := range []struct {
		name string
		info machine.DriverInfo
		kind error
	}{
		{"the same Driver, version and CPU", same, nil},
		{"another Driver", withInfo(same, func(i *machine.DriverInfo) { i.Name = "other" }), machine.ErrPreconditionFailed},
		{"another version", withInfo(same, func(i *machine.DriverInfo) { i.Version = "2" }), machine.ErrPreconditionFailed},
		{"another CPU", withInfo(same, func(i *machine.DriverInfo) { i.CPUModel = "cpu-b" }), machine.ErrPreconditionFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			o := e.elsewhere(tt.info)
			o.images.Provide(image)
			err := o.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)
			if tt.kind == nil {
				require.NoError(t, err)
				o.requireState(t, "new", machine.StateRunning)
				return
			}
			requireKind(t, err, tt.kind)
			o.requireNoMachine(t, "new")
			assert.Zero(t, o.driver.Calls("describe-restore"), "nothing was begun")
		})
	}
}

func withInfo(i machine.DriverInfo, change func(*machine.DriverInfo)) machine.DriverInfo {
	change(&i)
	return i
}

// Row "Restore", column Needs: the rest of what a restore needs.
func TestRestoreSnapshot_Needs(t *testing.T) {
	ctx := context.Background()

	t.Run("a snapshot the node holds", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r", "nope", narrowPolicy), machine.ErrNotFound)
		e.requireNoMachine(t, "new")
	})
	t.Run("the identifiers and a valid policy", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		requireKind(t, e.svc.RestoreSnapshot(ctx, "", "r", "snap", narrowPolicy), machine.ErrInvalid)
		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "", "snap", narrowPolicy), machine.ErrInvalid)
		bad := machine.NetworkPolicy{Egress: []machine.NetworkRule{{Protocol: machine.ProtocolTCP}}}
		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r", "snap", bad), machine.ErrInvalid)
		e.requireNoMachine(t, "new")
	})
	t.Run("the fencing deadline armed", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		e.permit.FailAt("permit", errBoom)

		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r", "snap", narrowPolicy), machine.ErrPreconditionFailed)
		e.requireNoMachine(t, "new")
		assert.Zero(t, e.disks.Calls("create-root-from"), "nothing was allocated")

		// Control: the same restore succeeds once the deadline permits it.
		e.permit.Heal("permit")
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r", "snap", narrowPolicy))
	})
	t.Run("the network policy applied", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		e.net.FailAt("apply", errBoom)

		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r", "snap", narrowPolicy), machine.ErrPreconditionFailed)
		e.requireNoMachine(t, "new")

		e.net.Heal("apply")
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r", "snap", narrowPolicy))
	})
	t.Run("the image on the node", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		o := e.elsewhere(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: snapshotCaps})

		requireKind(t, o.svc.RestoreSnapshot(ctx, "new", "r", "snap", narrowPolicy), machine.ErrPreconditionFailed)
		o.requireNoMachine(t, "new")
	})
}

// Row "Restore", column On success.
func TestRestoreSnapshot_OnSuccess(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	source := e.requireState(t, "src", machine.StateRunning)

	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
	got := e.requireState(t, "new", machine.StateRunning)

	assert.NotEqual(t, source.User.UID, got.User.UID, "a new UID")
	assert.Equal(t, got.User.UID, got.User.GID)
	assert.Equal(t, uint32(1001), got.User.UID, "allocated as Create allocates")
	assert.Equal(t, source.Network.MAC, got.Network.MAC, "the guest keeps the snapshot's MAC (ADR 0011)")
	assert.NotEqual(t, source.Network.Addresses, got.Network.Addresses, "new addresses")
	e.created(t, "fresh")
	assert.NotEqual(t, source.Network.MAC, e.requireState(t, "fresh", machine.StateCreated).Network.MAC, "control: a Create gets a MAC of its own")
	assert.Equal(t, narrowPolicy, got.Policy, "the policy is the request's and not the source's")
	assert.Equal(t, image, got.Image)
	assert.Equal(t, source.Resources, got.Resources)
	e.requireStored(t, "new", machine.StateRunning)

	assert.Equal(t, []machine.State{machine.StateRunning}, e.events.States("new"), "the state change is reported")

	// The root disk is a copy of the snapshot's, owned by the new UID.
	src, ok := e.disks.Source(got.RootDisk)
	require.True(t, ok)
	assert.Equal(t, "/disks/src-root", src)
	owner, _ := e.disks.Owner(got.RootDisk)
	assert.Equal(t, machine.DiskOwner{ID: "new", UID: got.User.UID, GID: got.User.UID}, owner)

	// The Driver was given the snapshot's files and the new Machine's own tap
	// and run directory, and the supervisor the same run directory.
	assert.Equal(t, []string{e.driver.BootSpec("src").RunDir + "/snapshot.mem", e.driver.BootSpec("src").RunDir + "/snapshot.state"}, e.driver.RestoredFrom("new"))
	boot := e.driver.BootSpec("new")
	assert.Equal(t, "tap-new", boot.Network.Tap)
	assert.Equal(t, got.Network.MAC, boot.Network.MAC)
	run, err := e.sup.RunDir("new")
	require.NoError(t, err)
	assert.Equal(t, run, boot.RunDir)
	process, running := e.sup.Running("new")
	require.True(t, running)
	assert.Equal(t, run, process.RunDir, "the supervisor's")
	user, _ := e.sup.User("new")
	assert.Equal(t, got.User, user, "the supervisor runs the VMM as the new user")

	// The guest holds the new Machine's addresses, gateways and resolvers, and
	// not the snapshot's.
	catchUps := e.guest.CatchUps("new")
	require.Len(t, catchUps, 1, "the guest was caught up once")
	sent := catchUps[0]
	assert.Equal(t, got.Network.Addresses, sent.Network.Addresses)
	assert.Equal(t, got.Network.Gateways, sent.Network.Gateways)
	assert.Equal(t, got.Network.Resolvers, sent.Network.Resolvers)
	assert.NotEmpty(t, sent.Network.Gateways)
	assert.NotEqual(t, source.Network.Addresses, sent.Network.Addresses)
	assert.Equal(t, machine.RequestID("r-new"), sent.RestoreID, "the restore ID is the request ID")
	assert.Equal(t, e.clock.Now(), sent.HostTime, "the host's wall clock")
	assert.GreaterOrEqual(t, len(sent.Entropy), 32, "entropy within ADR 0016's 32 to 256 bytes")
	assert.LessOrEqual(t, len(sent.Entropy), 256)
	assert.Empty(t, e.guest.CatchUps("src"), "control: the source's guest is not touched")

	// Control: the source is as it was.
	e.requireState(t, "src", machine.StateRunning)
	assert.Equal(t, source.User, e.requireState(t, "src", machine.StateRunning).User)
}

// Row "Restore", column Repeat.
func TestRestoreSnapshot_Repeat(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))

	require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), "the same request: the original outcome")
	assert.Equal(t, 1, e.disks.Calls("create-root-from"), "nothing was made a second time")

	requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "another", "snap", narrowPolicy), machine.ErrConflict)
	requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", machine.NetworkPolicy{}), machine.ErrConflict)
	require.NoError(t, e.svc.Snapshot(ctx, "src", "snap-2", "r-2"))
	requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap-2", narrowPolicy), machine.ErrConflict)

	t.Run("a failed restore repeats its failure and begins nothing", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		e.driver.FailAt("restore", errBoom)
		first := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)
		requireKind(t, first, machine.ErrFailed)
		calls := e.driver.Calls("describe-restore")

		e.driver.Heal("restore")
		again := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)
		assert.Equal(t, first, again, "the original outcome")
		assert.Equal(t, calls, e.driver.Calls("describe-restore"))

		// Control: another request ID for the same identifier is a new attempt.
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-second", "snap", narrowPolicy))
	})
}

// Row "Restore", column State after failure: no Machine exists, and what was
// allocated is released.
func TestRestoreSnapshot_StateAfterFailure(t *testing.T) {
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		fail func(*env)
		heal func(*env)
		kind error
	}{
		{"the network cannot be allocated", func(e *env) { e.net.FailAt("allocate", errBoom) }, func(e *env) { e.net.Heal("allocate") }, machine.ErrFailed},
		{"the root disk cannot be made", func(e *env) { e.disks.FailAt("create-root-from", errBoom) }, func(e *env) { e.disks.Heal("create-root-from") }, machine.ErrFailed},
		{"the network policy cannot be applied", func(e *env) { e.net.FailAt("apply", errBoom) }, func(e *env) { e.net.Heal("apply") }, machine.ErrPreconditionFailed},
		{"the Driver cannot describe the process", func(e *env) { e.driver.FailAt("describe-restore", errBoom) }, func(e *env) { e.driver.Heal("describe-restore") }, machine.ErrFailed},
		{"the supervisor cannot start", func(e *env) { e.sup.FailAt("start", errBoom) }, func(e *env) { e.sup.Heal("start") }, machine.ErrFailed},
		{"the snapshot cannot be loaded", func(e *env) { e.driver.FailAt("restore", errBoom) }, func(e *env) { e.driver.Heal("restore") }, machine.ErrFailed},
		{"the Machine cannot be recorded", func(e *env) { e.store.FailAt("save", errBoom) }, func(e *env) { e.store.Heal("save") }, machine.ErrFailed},
		{"the guest cannot be caught up", func(e *env) { e.guest.FailAt("catch-up", errBoom) }, func(e *env) { e.guest.Heal("catch-up") }, machine.ErrFailed},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(snapshotCaps...)
			e.snapshotted(t, "src", machine.StateRunning)
			tt.fail(e)

			requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), tt.kind)
			e.requireNoMachine(t, "new")
			_, diskHeld := e.disks.Holds("new-root")
			assert.False(t, diskHeld, "the root disk was removed")
			assert.False(t, e.net.Allocated("new"), "the addresses were released")
			_, running := e.sup.Running("new")
			assert.False(t, running, "no VMM process is left")
			_, tapped := e.net.Applied("new")
			assert.False(t, tapped, "no tap is left")

			// The UID went back: the next Machine gets the next one after the source's.
			tt.heal(e)
			e.created(t, "next")
			assert.Equal(t, uint32(1001), e.requireState(t, "next", machine.StateCreated).User.UID, "the UID was released")

			// Control: the restore itself succeeds once the step stops failing,
			// under another request ID where the failure was remembered.
			require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-again", "snap", narrowPolicy))
		})
	}

	for _, tt := range []struct {
		name string
		fail func(*env)
	}{
		{"the snapshot cannot be loaded", func(e *env) { e.driver.FailAt("restore", errBoom) }},
		{"the Machine cannot be recorded", func(e *env) { e.store.FailAt("save", errBoom) }},
		{"the guest cannot be caught up", func(e *env) { e.guest.FailAt("catch-up", errBoom) }},
	} {
		t.Run("a VMM that cannot be ended is a Faulted Machine that holds what it allocated: "+tt.name, func(t *testing.T) {
			e := newEnv(snapshotCaps...)
			e.snapshotted(t, "src", machine.StateRunning)
			tt.fail(e)
			e.sup.FailAt("terminate", errBoom)

			err := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)
			requireKind(t, err, machine.ErrFailed)
			assert.Contains(t, err.Error(), "could not be ended")
			m := e.requireState(t, "new", machine.StateFaulted)
			assert.Contains(t, m.FailureReason, "could not be ended")
			assert.Equal(t, []machine.State{machine.StateFaulted}, e.events.States("new"), "the Faulted state is reported")
			assert.True(t, e.net.Allocated("new"), "what it allocated is still its own")
			_, diskHeld := e.disks.Holds("new-root")
			assert.True(t, diskHeld)

			// A repeat is the original outcome, and does not allocate again.
			assert.Equal(t, err, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
			assert.Equal(t, 1, e.disks.Calls("create-root-from"))

			// Delete releases all of it, as for every Faulted Machine.
			e.sup.Heal("terminate")
			e.store.Heal("save")
			require.NoError(t, e.svc.Delete(ctx, "new"))
			e.requireNoMachine(t, "new")
			_, running := e.sup.Running("new")
			assert.False(t, running)
			_, tapped := e.net.Applied("new")
			assert.False(t, tapped)
			e.created(t, "next")
			assert.Equal(t, uint32(1001), e.requireState(t, "next", machine.StateCreated).User.UID, "the UID was released")
		})
	}
}

// Two restores of one snapshot send their guests different entropy, each under
// its own restore ID: clones that were sent the same bytes would share random
// state again (ADR 0016).
func TestRestoreSnapshot_EachRestoreSendsEntropyOfItsOwn(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	require.NoError(t, e.svc.RestoreSnapshot(ctx, "a", "r-a", "snap", narrowPolicy))
	e.clock.Set(e.clock.Now().Add(time.Hour))
	require.NoError(t, e.svc.RestoreSnapshot(ctx, "b", "r-b", "snap", narrowPolicy))

	a, b := e.guest.CatchUps("a"), e.guest.CatchUps("b")
	require.Len(t, a, 1)
	require.Len(t, b, 1)
	assert.NotEqual(t, a[0].Entropy, b[0].Entropy, "fresh bytes for each restore")
	assert.NotEqual(t, make([]byte, len(a[0].Entropy)), a[0].Entropy, "and not zeroes")
	assert.Equal(t, machine.RequestID("r-a"), a[0].RestoreID)
	assert.Equal(t, machine.RequestID("r-b"), b[0].RestoreID)
	assert.Equal(t, b[0].HostTime.Sub(a[0].HostTime), time.Hour, "each takes the host's time when it is sent")
}

// The Machine is not Running, nor reported so, until its guest has caught up.
func TestRestoreSnapshot_IsNotRunningUntilTheGuestHasItsNetwork(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.snapshotted(t, "src", machine.StateRunning)
	entered, release := e.guest.Hold("catch-up")
	done := make(chan error, 1)
	go func() { done <- e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the restore never sent the guest its network")
	}

	_, err := e.svc.Inspect(ctx, "new")
	requireKind(t, err, machine.ErrNotFound)
	assert.Empty(t, e.events.States("new"), "no state is reported while the guest has the old addresses")

	release()
	require.NoError(t, <-done)
	e.requireState(t, "new", machine.StateRunning)
}

// A snapshot is not visible to a restore while Snapshot is still taking it:
// the restore waits for it, and finds no snapshot when it fails. Many restores
// of one snapshot run at once.
func TestRestoreSnapshot_WaitsForTheSnapshotToFinish(t *testing.T) {
	ctx := context.Background()

	// restoring runs a restore on its own goroutine and returns its result.
	restoring := func(e *env, id machine.MachineID) <-chan error {
		done := make(chan error, 1)
		go func() { done <- e.svc.RestoreSnapshot(ctx, id, "r-"+machine.RequestID(id), "snap", narrowPolicy) }()
		return done
	}
	requireWaiting := func(t *testing.T, done <-chan error) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("the restore finished while the snapshot was being taken: %v", err)
		case <-time.After(100 * time.Millisecond):
		}
	}

	t.Run("a Snapshot that fails", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "src", machine.StateRunning)
		entered, release := e.driver.Hold("resume")
		e.driver.FailAt("resume", errBoom)
		snapshotting := make(chan error, 1)
		go func() { snapshotting <- e.svc.Snapshot(ctx, "src", "snap", "r-snap") }()
		<-entered
		require.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held(), "the setup: the store holds it while the guest is still paused")

		restored := restoring(e, "new")
		requireWaiting(t, restored)
		release()

		requireKind(t, <-snapshotting, machine.ErrFailed)
		requireKind(t, <-restored, machine.ErrNotFound)
		e.requireNoMachine(t, "new")
	})
	t.Run("a Snapshot that succeeds", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.inState(t, "src", machine.StateRunning)
		entered, release := e.driver.Hold("resume")
		snapshotting := make(chan error, 1)
		go func() { snapshotting <- e.svc.Snapshot(ctx, "src", "snap", "r-snap") }()
		<-entered

		restored := restoring(e, "new")
		requireWaiting(t, restored)
		release()

		require.NoError(t, <-snapshotting)
		require.NoError(t, <-restored)
		e.requireState(t, "new", machine.StateRunning)
	})
	t.Run("restores of one snapshot run together", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.snapshotted(t, "src", machine.StateRunning)
		entered, release := e.driver.Hold("restore")
		first := restoring(e, "first")
		<-entered

		second := restoring(e, "second")
		select {
		case err := <-second:
			require.NoError(t, err)
		case <-time.After(2 * time.Second):
			release()
			t.Fatal("a second restore of the snapshot was held by the first")
		}
		release()
		require.NoError(t, <-first)
	})
}
