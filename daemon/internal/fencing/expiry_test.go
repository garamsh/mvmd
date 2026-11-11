package fencing_test

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

func spec() machine.Spec {
	return machine.Spec{
		Image:         image,
		Resources:     machine.Resources{VCPUs: 1, MemoryBytes: 2 << 20},
		RootDiskBytes: 1 << 20,
	}
}

// runtime-api.md: "Start and restore are refused while the deadline is not
// armed or has expired." (Restore arrives with its own issue.)
func TestStart_RefusedUnlessArmed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	require.NoError(t, e.m.Create(ctx, "m", "r", spec()))

	t.Run("unarmed", func(t *testing.T) {
		requireKind(t, e.m.Start(ctx, "m"), machine.ErrPreconditionFailed)
		_, alive := e.sup.Expiry("m")
		assert.False(t, alive, "no supervisor, so no VMM, without an expiry")
		assert.Zero(t, e.driver.Calls("boot"))
	})

	t.Run("armed", func(t *testing.T) {
		e.renew(t, time.Minute, 1)
		require.NoError(t, e.m.Start(ctx, "m"))
	})

	t.Run("expired", func(t *testing.T) {
		require.NoError(t, e.m.Stop(ctx, "m", time.Second))
		e.clock.Advance(time.Minute)
		requireKind(t, e.m.Start(ctx, "m"), machine.ErrPreconditionFailed)
		assert.Equal(t, machine.StateStopped, e.state(t, "m"))
		_, alive := e.sup.Expiry("m")
		assert.False(t, alive)
	})

	t.Run("a renewal arms it again", func(t *testing.T) {
		e.renew(t, time.Minute, 2)
		require.NoError(t, e.m.Start(ctx, "m"))
	})
}

// holdSnapshot makes the node hold a snapshot that this node's Driver took.
func (e *env) holdSnapshot(t *testing.T) {
	t.Helper()
	require.NoError(t, e.snaps.Put(context.Background(), machine.Snapshot{
		ID: "snap", Machine: "gone", Request: "r",
		Spec:         machine.SnapshotSpec{Image: image, Resources: machine.Resources{VCPUs: 1, MemoryBytes: 2 << 20}, RootDiskBytes: 1 << 20},
		RootDiskUUID: "uuid",
		Compat:       machine.SnapshotCompat{Driver: "inmem", Version: "1", CPUModel: "cpu-a", KernelDigest: machinememory.KernelDigest},
	}, machine.SnapshotFiles{State: []string{"/snap/mem"}, RootDisk: "/snap/root"}))
}

// Restore is gated as Start is: refused while the deadline is unarmed or
// expired, so no supervisor and no VMM exist for it. The control is the same
// restore under an armed deadline.
func TestRestoreSnapshot_IsRefusedWithoutAnArmedDeadline(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.holdSnapshot(t)

	t.Run("unarmed", func(t *testing.T) {
		requireKind(t, e.m.RestoreSnapshot(ctx, "new", "r-1", "snap", machine.NetworkPolicy{}), machine.ErrPreconditionFailed)
		_, alive := e.sup.Expiry("new")
		assert.False(t, alive, "no supervisor, so no VMM, without an expiry")
		assert.Zero(t, e.driver.Calls("restore"))
	})
	t.Run("armed", func(t *testing.T) {
		e.renew(t, time.Minute, 1)
		require.NoError(t, e.m.RestoreSnapshot(ctx, "new", "r-2", "snap", machine.NetworkPolicy{}))
		assert.Equal(t, machine.StateRunning, e.state(t, "new"))
	})
	t.Run("expired", func(t *testing.T) {
		e.clock.Advance(time.Minute)
		requireKind(t, e.m.RestoreSnapshot(ctx, "other", "r-3", "snap", machine.NetworkPolicy{}), machine.ErrPreconditionFailed)
		_, alive := e.sup.Expiry("other")
		assert.False(t, alive)
	})
}

// A restored Machine's supervisor is started with the expiry in force, in the
// same step as the permission, and later renewals reach it.
func TestRestoreSnapshot_SupervisorIsStartedWithTheCurrentExpiry(t *testing.T) {
	e := newEnv(t)
	e.holdSnapshot(t)
	e.renew(t, time.Minute, 1)
	e.clock.Advance(10 * time.Second)
	e.renew(t, time.Minute, 2)
	require.NoError(t, e.m.RestoreSnapshot(context.Background(), "new", "r", "snap", machine.NetworkPolicy{}))

	rec, _ := e.store.Recorded()
	expiry, alive := e.sup.Expiry("new")
	require.True(t, alive)
	assert.Equal(t, rec.Expiry, expiry)
	process, _ := e.sup.Process("new")
	assert.Equal(t, "/usr/libexec/inmem", process.Executable, "the supervisor was given the Driver's description")
	assert.NotEmpty(t, process.RunDir)

	e.renew(t, time.Hour, 3)
	e.requireSupervisorsAt(t)
}

// ADR 0003: "No VMM without an expiry": Start hands the supervisor the
// current expiry, and later renewals reach it.
func TestStart_SupervisorIsStartedWithTheCurrentExpiry(t *testing.T) {
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.clock.Advance(10 * time.Second)
	e.renew(t, time.Minute, 2)
	e.running(t, "a")

	rec, _ := e.store.Recorded()
	expiry, alive := e.sup.Expiry("a")
	require.True(t, alive)
	assert.Equal(t, rec.Expiry, expiry)
	process, _ := e.sup.Process("a")
	assert.Equal(t, "/usr/libexec/inmem", process.Executable, "the supervisor was given the Driver's description")
	user, _ := e.sup.User("a")
	assert.NotZero(t, user.UID, "and the Machine's user, never root")
	assert.Empty(t, e.store.Violations())

	e.renew(t, 2*time.Minute, 3)
	e.requireSupervisorsAt(t)
}

// A start whose supervisor start fails leaves no supervisor.
func TestStart_FailureLeavesNoSupervisor(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	require.NoError(t, e.m.Create(ctx, "m", "r", spec()))
	e.sup.FailAt("start", errBoom)

	requireKind(t, e.m.Start(ctx, "m"), machine.ErrFailed)
	_, alive := e.sup.Expiry("m")
	assert.False(t, alive)
	assert.Equal(t, machine.StateCreated, e.state(t, "m"))
}

// runtime-api.md: "When the deadline passes, mvmd stops every Machine under it
// by force, with no wait for the guest ... Disks are retained. mvmd then
// starts nothing by itself; a later renewal re-arms the deadline."
func TestExpiry_StopsEveryMachineByForce(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.running(t, "a", "b", "c")
	require.NoError(t, e.m.Pause(ctx, "b"))
	require.NoError(t, e.m.Create(ctx, "idle", "r", spec())) // Created: no VMM
	stop := e.run(t)
	defer stop()

	e.clock.Advance(59 * time.Second)
	assert.Equal(t, machine.StateRunning, e.state(t, "a"), "before the expiry nothing stops")

	e.clock.Advance(time.Second)
	e.eventually(t, "every Machine is stopped", func() bool {
		return e.state(t, "a") == machine.StateStopped &&
			e.state(t, "b") == machine.StateStopped &&
			e.state(t, "c") == machine.StateStopped
	})
	live, err := e.sup.Live(ctx)
	require.NoError(t, err)
	assert.Empty(t, live, "every supervisor was killed")
	assert.Zero(t, e.driver.Calls("shutdown"), "the guest was not asked, and not waited for")
	for _, id := range []machine.MachineID{"a", "b", "c", "idle"} {
		_, held := e.disks.Holds(machine.DiskID(string(id) + "-root"))
		assert.True(t, held, "the disk of %q is retained", id)
	}
	assert.Equal(t, machine.StateCreated, e.state(t, "idle"))
	e.eventually(t, "the phase", func() bool { return e.status(t).Phase == fencing.PhaseStopped })

	// Nothing restarts by itself, and a start is refused until a renewal.
	requireKind(t, e.m.Start(ctx, "a"), machine.ErrPreconditionFailed)
	e.clock.Advance(time.Hour)
	assert.Equal(t, machine.StateStopped, e.state(t, "a"))
	e.renew(t, time.Minute, 2)
	assert.Equal(t, machine.StateStopped, e.state(t, "a"), "a renewal does not start anything")
	require.NoError(t, e.m.Start(ctx, "a"))
}

// ADR 0003, host suspended across the expiry: the clock counts the suspension,
// so one jump past the expiry stops everything; and a renewal before the
// expiry moves it.
func TestExpiry_FollowsRenewalsAndJumps(t *testing.T) {
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.running(t, "a")
	stop := e.run(t)
	defer stop()

	e.clock.Advance(50 * time.Second)
	e.renew(t, time.Minute, 2) // the old expiry, ten seconds ahead, is now void
	e.clock.Advance(30 * time.Second)
	// Only the first expiry has passed. Give Run the chance to act wrongly.
	time.Sleep(20 * time.Millisecond)
	assert.Equal(t, machine.StateRunning, e.state(t, "a"))

	e.clock.Advance(24 * time.Hour) // a suspend: one step, far past the expiry
	e.eventually(t, "stopped after the jump", func() bool { return e.state(t, "a") == machine.StateStopped })
}

// A supervisor that cannot be killed at the expiry is reported, and the phase
// does not claim everything is stopped.
func TestExpiry_KillFailureIsReported(t *testing.T) {
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.running(t, "a")
	e.sup.FailAt("terminate:a", errBoom)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	e.clock.Advance(time.Minute)

	select {
	case err := <-done:
		require.ErrorIs(t, err, errBoom)
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not report the failed kill")
	}
	assert.Equal(t, fencing.PhaseExpired, e.status(t).Phase)
}

// runtime-api.md: "On start, before it serves any call, mvmd reattaches ...
// if the expiry has passed, it stops every Machine first; if not, the remaining
// time is what is left of the recorded expiry, never the full length. If the
// host has rebooted, no VM survived, and the deadline is treated as not armed."
func TestReconcile(t *testing.T) {
	ctx := context.Background()

	t.Run("no record: not armed", func(t *testing.T) {
		e := newEnv(t)
		require.NoError(t, e.restart(t))
		assert.Equal(t, fencing.PhaseUnarmed, e.status(t).Phase)
	})

	t.Run("a different boot_id: not armed, and nothing starts until a renewal", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		e.clock.Reboot("boot-2")
		require.NoError(t, e.restart(t))

		assert.Equal(t, fencing.PhaseUnarmed, e.status(t).Phase)
		requireKind(t, e.svc.PermitStart(ctx, "m"), machine.ErrPreconditionFailed)

		// Control: after a renewal the new boot is armed.
		st, err := e.svc.Renew(ctx, time.Minute, 1)
		require.NoError(t, err, "a renewal number is accepted after a reboot, the record being of another boot")
		assert.Equal(t, fencing.PhaseArmed, st.Phase)
	})

	t.Run("a passed expiry stops everything first and is not extended", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		e.running(t, "a", "b")
		e.clock.Advance(2 * time.Minute) // mvmd was down across the expiry

		require.NoError(t, e.restart(t))
		live, err := e.sup.Live(ctx)
		require.NoError(t, err)
		assert.Empty(t, live)
		assert.Equal(t, machine.StateStopped, e.state(t, "a"))
		assert.Equal(t, machine.StateStopped, e.state(t, "b"))
		assert.Equal(t, fencing.PhaseStopped, e.status(t).Phase)
		rec, _ := e.store.Recorded()
		assert.Less(t, rec.Expiry, e.clock.Now(), "the recorded expiry was not moved")
		requireKind(t, e.svc.PermitStart(ctx, "a"), machine.ErrPreconditionFailed)
	})

	t.Run("a live expiry: each supervisor gets the recorded one, and the time left is what is left of it", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, 10*time.Second, 1)
		e.running(t, "a", "b")
		e.clock.Advance(4 * time.Second)

		require.NoError(t, e.restart(t))
		st := e.status(t)
		assert.Equal(t, fencing.PhaseArmed, st.Phase)
		assert.Equal(t, 6*time.Second, st.Remaining, "not the full ten seconds")
		assert.Equal(t, 10*time.Second, st.Length)
		e.requireSupervisorsAt(t)
		assert.Equal(t, machine.StateRunning, e.state(t, "a"))
	})

	t.Run("a supervisor that survived a renewal that did not reach it is brought to the record", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		e.running(t, "a")
		e.sup.FailAt("set-expiry", errBoom) // mvmd "crashes" during the extension
		_, err := e.svc.Renew(ctx, time.Hour, 2)
		requireKind(t, err, machine.ErrFailed)
		e.sup.Heal("set-expiry")

		require.NoError(t, e.restart(t))
		e.requireSupervisorsAt(t)
		assert.Equal(t, time.Hour, e.status(t).Remaining)
	})

	t.Run("a shortening that died before its record leaves supervisors at or below the record, and reconcile passes the record", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Hour, 1)
		e.running(t, "a")
		e.store.FailAt("save", errBoom)
		_, err := e.svc.Renew(ctx, time.Minute, 2)
		requireKind(t, err, machine.ErrFailed)
		e.store.Heal("save")

		require.NoError(t, e.restart(t))
		assert.Empty(t, e.store.Violations())
		e.requireSupervisorsAt(t)
	})

	t.Run("an acknowledged renewal is held by every supervisor before and after a restart", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		e.running(t, "a", "b")
		e.renew(t, 5*time.Minute, 2)
		e.requireSupervisorsAt(t)

		require.NoError(t, e.restart(t))
		e.requireSupervisorsAt(t)
	})

	t.Run("a supervisor mvmd cannot reach keeps what it holds, no later than the record", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		e.running(t, "a", "b")
		e.sup.FailAt("set-expiry:b", errBoom)

		require.NoError(t, e.restart(t))
		rec, _ := e.store.Recorded()
		held, alive := e.sup.Expiry("b")
		require.True(t, alive)
		assert.LessOrEqual(t, held, rec.Expiry)
		assert.Empty(t, e.store.Violations())
	})

	t.Run("a supervisor started by an mvmd that died while starting the Machine is reattached", func(t *testing.T) {
		e := newEnv(t)
		e.renew(t, time.Minute, 1)
		require.NoError(t, e.m.Create(ctx, "a", "r", spec()))
		e.driver.FailAt("boot", errBoom)
		e.sup.FailAt("terminate", errBoom) // the dying mvmd never cleaned up
		require.Error(t, e.m.Start(ctx, "a"))
		_, alive := e.sup.Expiry("a")
		require.True(t, alive, "the supervisor exists and holds the expiry it was started with")

		e.clock.Advance(20 * time.Second)
		require.NoError(t, e.restart(t))
		e.requireSupervisorsAt(t)
		assert.Equal(t, 40*time.Second, e.status(t).Remaining)
	})

	t.Run("the record cannot be read: reconcile fails and nothing is served", func(t *testing.T) {
		e := newEnv(t)
		e.store.FailAt("load", errBoom)
		require.ErrorIs(t, e.restart(t), errBoom)
		_, err := e.svc.Renew(ctx, time.Minute, 1)
		requireKind(t, err, machine.ErrPreconditionFailed)
	})
}

// ADR 0003: "Starts and renewals are applied one at a time, so every
// supervisor that exists when a renewal is applied receives it."
func TestStartsAndRenewalsAreSerialized(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.renew(t, time.Hour, 1)

	const machines = 24
	for i := range machines {
		id := machine.MachineID(fmt.Sprintf("m%d", i))
		require.NoError(t, e.m.Create(ctx, id, machine.RequestID("r-"+id), spec()))
	}

	var number atomic.Uint64
	number.Store(1)
	var wg sync.WaitGroup
	for i := range machines {
		wg.Add(1)
		go func() {
			defer wg.Done()
			require.NoError(t, e.m.Start(ctx, machine.MachineID(fmt.Sprintf("m%d", i))))
		}()
	}
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			length := time.Hour
			if i%2 == 0 {
				length = time.Minute // alternating extensions and shortenings
			}
			_, err := e.svc.Renew(ctx, length, number.Add(1))
			require.NoError(t, err)
		}()
	}
	wg.Wait()

	live, err := e.sup.Live(ctx)
	require.NoError(t, err)
	assert.Len(t, live, machines)
	e.requireSupervisorsAt(t)
}

// B1 of the lifecycle review: a renewal never waits behind a Machine's long
// call. A renewal delayed past the client's margin would stop every Machine.
func TestRenewal_DoesNotWaitBehindALongStop(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.renew(t, time.Minute, 1)
	e.running(t, "a", "b")
	entered, release := e.driver.HoldShutdown()
	stopped := make(chan error, 1)
	go func() { stopped <- e.m.Stop(ctx, "a", time.Hour) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never reached the guest")
	}

	renewed := make(chan error, 1)
	go func() {
		_, err := e.svc.Renew(ctx, 2*time.Minute, 2)
		if err == nil {
			_, err = e.svc.Inspect(ctx)
		}
		renewed <- err
	}()
	select {
	case err := <-renewed:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the renewal waited for a Machine's stop grace period")
	}
	e.requireSupervisorsAt(t)

	release()
	require.NoError(t, <-stopped)
}
