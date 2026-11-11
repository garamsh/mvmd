package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
)

var stable, next = machine.KernelFile{Path: "/kernels/stable", SHA256: "digest-stable"}, machine.KernelFile{Path: "/kernels/next", SHA256: "digest-next"}

// twoKernels is a node that offers two kernels and defaults to "stable".
var twoKernels = machine.Kernels{Default: "stable", Offered: map[string]machine.KernelFile{"stable": stable, "next": next}}

// withKernels is the env's service over the kernels, sharing its store.
func (e *env) withKernels(t *testing.T, k machine.Kernels) machine.Service {
	t.Helper()
	return e.reopenedWith(t, machine.WithKernels(k))
}

func (e *env) createdWithKernel(t *testing.T, id machine.MachineID, kernel string) error {
	t.Helper()
	spec := baseSpec()
	spec.Kernel = kernel
	return e.svc.Create(context.Background(), id, request(id), spec)
}

func TestCreate_RecordsTheKernel(t *testing.T) {
	t.Run("none asked records the node's default", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.createdWithKernel(t, "m", ""))
		assert.Equal(t, "stable", e.requireState(t, "m", machine.StateCreated).Kernel)
		rec, _ := e.store.Record("m")
		assert.Equal(t, "stable", rec.Machine.Kernel, "the record keeps it")
	})

	t.Run("a named kernel is recorded", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.createdWithKernel(t, "m", "next"))
		assert.Equal(t, "next", e.requireState(t, "m", machine.StateCreated).Kernel)
	})

	t.Run("a name the node does not offer is invalid and nothing is recorded", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		requireKind(t, e.createdWithKernel(t, "m", "nope"), machine.ErrInvalid)
		_, held := e.store.Record("m")
		assert.False(t, held)
		require.NoError(t, e.createdWithKernel(t, "m", "next"), "control: the same Create with an offered name succeeds")
	})

	t.Run("a repeat with no name still matches after the default moved", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.createdWithKernel(t, "m", ""))
		moved := machine.Kernels{Default: "next", Offered: twoKernels.Offered}
		e.svc = e.withKernels(t, moved)
		require.NoError(t, e.createdWithKernel(t, "m", ""))
		assert.Equal(t, "stable", e.requireState(t, "m", machine.StateCreated).Kernel, "the Machine did not move")
	})
}

func TestStart_BootsTheFileTheRecordedNameMapsTo(t *testing.T) {
	ctx := context.Background()

	t.Run("the named file and digest", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.createdWithKernel(t, "m", "next"))
		require.NoError(t, e.svc.Start(ctx, "m"))
		assert.Equal(t, next, e.driver.BootSpec("m").Kernel)
		assert.Equal(t, "digest-next", e.requireState(t, "m", machine.StateRunning).KernelDigest)
	})

	t.Run("the file behind a name as it is at the start", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.createdWithKernel(t, "m", "next"))
		updated := machine.KernelFile{Path: "/kernels/next-2", SHA256: "digest-next-2"}
		e.svc = e.withKernels(t, machine.Kernels{Default: "stable", Offered: map[string]machine.KernelFile{"stable": stable, "next": updated}})
		require.NoError(t, e.svc.Start(ctx, "m"))
		assert.Equal(t, updated, e.driver.BootSpec("m").Kernel)
	})

	t.Run("a record with no name boots the node's default", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.createdWithKernel(t, "m", "next"))
		rec, _ := e.store.Record("m")
		rec.Machine.Kernel = ""
		require.NoError(t, e.store.Save(ctx, rec))
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.svc.Start(ctx, "m"))
		assert.Equal(t, stable, e.driver.BootSpec("m").Kernel)
	})

	t.Run("a name the node no longer offers is a failed precondition and the record is kept", func(t *testing.T) {
		e := newEnv()
		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.createdWithKernel(t, "m", "next"))
		e.svc = e.withKernels(t, machine.Kernels{Default: "stable", Offered: map[string]machine.KernelFile{"stable": stable}})
		err := e.svc.Start(ctx, "m")
		requireKind(t, err, machine.ErrPreconditionFailed)
		assert.Contains(t, err.Error(), `"next"`)
		assert.Equal(t, "next", e.requireState(t, "m", machine.StateCreated).Kernel)
		rec, held := e.store.Record("m")
		require.True(t, held)
		assert.Equal(t, "next", rec.Machine.Kernel)
		assert.Zero(t, e.driver.Calls("describe"), "nothing was started")

		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.svc.Start(ctx, "m"), "control: the node offering the name again starts it")
	})
}

// kernelSnapshot is a snapshot of a Machine that booted the named kernel.
func (e *env) kernelSnapshot(t *testing.T, kernel string) {
	t.Helper()
	require.NoError(t, e.createdWithKernel(t, "src", kernel))
	require.NoError(t, e.svc.Start(context.Background(), "src"))
	require.NoError(t, e.svc.Snapshot(context.Background(), "src", "snap", "r-snap"))
}

func TestRestoreSnapshot_RequiresTheKernelTheSnapshotRecorded(t *testing.T) {
	ctx := context.Background()

	t.Run("a node that lacks the digest refuses and writes nothing", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.svc = e.withKernels(t, twoKernels)
		e.kernelSnapshot(t, "next")
		e.svc = e.withKernels(t, machine.Kernels{Default: "stable", Offered: map[string]machine.KernelFile{"stable": stable}})

		requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", machine.NetworkPolicy{}), machine.ErrPreconditionFailed)
		_, held := e.store.Record("new")
		assert.False(t, held, "no record")
		assert.Zero(t, e.driver.Calls("describe-restore"))
		disks, err := e.svc.ListDisks(ctx)
		require.NoError(t, err)
		for _, d := range disks {
			assert.NotEqual(t, machine.RootDiskID("new"), d.ID, "no root disk")
		}

		e.svc = e.withKernels(t, twoKernels)
		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", machine.NetworkPolicy{}), "control: a node offering the digest restores")
	})

	t.Run("a kernel under another name matches and its name is recorded", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.svc = e.withKernels(t, twoKernels)
		e.kernelSnapshot(t, "next")
		renamed := machine.Kernels{Default: "stable", Offered: map[string]machine.KernelFile{"stable": stable, "candidate": next}}
		e.svc = e.withKernels(t, renamed)

		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", machine.NetworkPolicy{}))
		m := e.requireState(t, "new", machine.StateRunning)
		assert.Equal(t, "candidate", m.Kernel)
		assert.Equal(t, "digest-next", m.KernelDigest)
		rec, _ := e.store.Record("new")
		assert.Equal(t, "candidate", rec.Machine.Kernel, "the record keeps it")
		assert.Empty(t, e.driver.RestoredWith("new").Kernel, "a restore boots no kernel")
	})

	t.Run("of two names with the digest the recorded one wins", func(t *testing.T) {
		e := newEnv(snapshotCaps...)
		e.svc = e.withKernels(t, twoKernels)
		e.kernelSnapshot(t, "next")
		twins := machine.Kernels{Default: "stable", Offered: map[string]machine.KernelFile{"stable": stable, "aaa": next, "next": next}}
		e.svc = e.withKernels(t, twins)

		require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", machine.NetworkPolicy{}))
		assert.Equal(t, "next", e.requireState(t, "new", machine.StateRunning).Kernel)
	})
}

// A Machine whose record carries no kernel digest (written before the digest was
// kept) cannot be snapshotted: a restore needs the digest, so the snapshot would
// be one no node could restore. The refusal writes nothing and says a restart
// records one (ADR 0045).
func TestSnapshot_RefusesAMachineWithNoRecordedKernelDigest(t *testing.T) {
	ctx := context.Background()
	e := newEnv(snapshotCaps...)
	e.inState(t, "m", machine.StateRunning)
	rec, ok := e.store.Record("m")
	require.True(t, ok)
	legacy := *rec.Machine
	legacy.KernelDigest = ""
	rec.Machine = &legacy
	require.NoError(t, e.store.Save(ctx, rec))

	svc := e.reopenedWith(t)
	err := svc.Snapshot(ctx, "m", "snap", "r-snap")
	requireKind(t, err, machine.ErrPreconditionFailed)
	assert.ErrorContains(t, err, "restart")
	assert.Empty(t, e.snaps.Held(), "nothing is written")
	assert.False(t, e.driver.PausedAtSnapshot("m"), "the guest was not paused for it")

	require.NoError(t, svc.Stop(ctx, "m", time.Second))
	require.NoError(t, svc.Start(ctx, "m"))
	require.NoError(t, svc.Snapshot(ctx, "m", "snap", "r-snap"), "control: after a start the same Machine records a digest and snapshots")
	assert.Equal(t, []machine.SnapshotID{"snap"}, e.snaps.Held())
}
