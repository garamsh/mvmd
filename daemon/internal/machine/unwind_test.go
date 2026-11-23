package machine_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// fenceBook is a machine.Fences that records who holds a fence, and fails at the step it is told to.
type fenceBook struct {
	fenced      map[machine.MachineID]bool
	enrolFails  error
	releasedAll int
}

func (h *fenceBook) Enrol(_ context.Context, id machine.MachineID) error {
	if h.enrolFails != nil {
		return h.enrolFails
	}
	h.fenced[id] = true
	return nil
}

func (h *fenceBook) Release(_ context.Context, id machine.MachineID) error {
	delete(h.fenced, id)
	h.releasedAll++
	return nil
}

func (h *fenceBook) Lapse(context.Context, machine.MachineID) (_ time.Duration, _ bool, _ error) {
	return 0, false, nil
}

// withUIDs rebuilds the service over the same adapters, with the allocator of UIDs and the fences
// the test reads back.
func (e *env) withUIDs(t *testing.T, uids *machine.UIDs, fences machine.Fences) {
	t.Helper()
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, uids, e.events,
		machine.WithKernels(memory.Kernels()), machine.WithDiskSnapshots(e.dsnaps), machine.WithFences(fences))
	require.NoError(t, e.svc.Restore(context.Background()))
}

// requireNothingTaken is the state a Create or a RestoreSnapshot that made no Machine leaves: no
// UID, no network, no root disk, the attached disks the node's again, and every holder slot free.
func requireNothingTaken(t *testing.T, e *env, uids *machine.UIDs, baseline int, id machine.MachineID, attached ...machine.DiskID) {
	t.Helper()
	e.requireNoMachine(t, id)
	assert.Equal(t, baseline, uids.Held(), "the UID of %q was given back", id)
	assert.False(t, e.net.Allocated(id), "the network of %q was released", id)
	_, rootHeld := e.disks.Holds(machine.RootDiskID(id))
	assert.False(t, rootHeld, "the root disk of %q was removed", id)
	for _, d := range attached {
		_, owned := e.disks.Owner(d)
		assert.False(t, owned, "the file of %q is mvmd's again", d)
	}
	e.requireNoDiskHolder(t, attached...)
}

// Every exit of Create that leaves no Machine gives back what the steps before it took (ADR 0009,
// ADR 0038): the UID, the network, the root disk, the disks given to the Machine, its fence and the
// holder slots.
func TestCreate_EveryExitThatLeavesNoMachineGivesBackWhatItTook(t *testing.T) {
	ctx := context.Background()
	steps := []struct {
		name string
		fail func(*env, *fenceBook)
		heal func(*env, *fenceBook)
		kind error
	}{
		{"the network cannot be allocated", func(e *env, _ *fenceBook) { e.net.FailAt("allocate", errBoom) }, func(e *env, _ *fenceBook) { e.net.Heal("allocate") }, machine.ErrFailed},
		{"the node has no free address", func(e *env, _ *fenceBook) { e.created(t, "other"); e.net.LimitAddresses(1) }, func(e *env, _ *fenceBook) { e.net.LimitAddresses(0) }, machine.ErrPreconditionFailed},
		{"the root disk cannot be made", func(e *env, _ *fenceBook) { e.disks.FailAt("create-root", errBoom) }, func(e *env, _ *fenceBook) { e.disks.Heal("create-root") }, machine.ErrFailed},
		{"the second disk cannot be given", func(e *env, _ *fenceBook) { e.disks.FailAfter("give", e.disks.Calls("give")+1, errBoom) }, func(e *env, _ *fenceBook) { e.disks.Heal("give") }, machine.ErrFailed},
		{"the fence cannot be made", func(_ *env, f *fenceBook) { f.enrolFails = errBoom }, func(_ *env, f *fenceBook) { f.enrolFails = nil }, machine.ErrFailed},
		{"the Machine cannot be recorded", func(e *env, _ *fenceBook) { e.store.FailAt("save", errBoom) }, func(e *env, _ *fenceBook) { e.store.Heal("save") }, machine.ErrFailed},
	}
	for _, tt := range steps {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(hotplugAndResize...)
			uids := machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256})
			fences := &fenceBook{fenced: map[machine.MachineID]bool{}}
			e.withUIDs(t, uids, fences)
			e.disks.Provide("d1")
			e.disks.Provide("d2")
			spec := baseSpec()
			spec.Fenced = true
			spec.Disks = []machine.DiskAttachment{rw("d1"), rw("d2")}
			tt.fail(e, fences)
			baseline := uids.Held()

			err := e.svc.Create(ctx, "m", "r", spec)
			requireKind(t, err, tt.kind)

			requireNothingTaken(t, e, uids, baseline, "m", "d1", "d2")
			assert.Empty(t, fences.fenced, "the fence of the Machine was removed")

			// Control: once the step works, the same call makes the Machine, so the refusal was the step's.
			tt.heal(e, fences)
			require.NoError(t, e.svc.Create(ctx, "m", "r2", spec))
			assert.Equal(t, baseline+1, uids.Held())
			assert.True(t, fences.fenced["m"])
		})
	}

	t.Run("a Fenced Create on a node with no fences is refused before anything is taken", func(t *testing.T) {
		e := newEnv(hotplugAndResize...)
		uids := machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256})
		e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, uids, e.events, machine.WithKernels(memory.Kernels()))
		e.disks.Provide("d1")
		spec := baseSpec()
		spec.Fenced = true
		spec.Disks = []machine.DiskAttachment{rw("d1")}

		requireKind(t, e.svc.Create(ctx, "m", "r", spec), machine.ErrUnsupported)

		assert.Zero(t, e.net.Calls("allocate"), "nothing was taken from the network")
		assert.Zero(t, e.disks.Calls("create-root"), "no root disk was made")
		assert.Zero(t, e.disks.Calls("give"), "no disk was given")
		requireNothingTaken(t, e, uids, 0, "m", "d1")

		spec.Fenced = false
		require.NoError(t, e.svc.Create(ctx, "m", "r2", spec), "control: the same Machine without the fence is made")
	})
}

// The same holds for RestoreSnapshot: every exit that leaves no Machine gives back the UID, the
// network, the root disk and the disks it took, whichever step it failed at.
func TestRestoreSnapshot_EveryExitThatLeavesNoMachineGivesBackWhatItTook(t *testing.T) {
	ctx := context.Background()
	steps := []struct {
		name string
		fail func(*env)
		heal func(*env)
		kind error
	}{
		{"the network cannot be allocated", func(e *env) { e.net.FailAt("allocate", errBoom) }, func(e *env) { e.net.Heal("allocate") }, machine.ErrFailed},
		{"the node has no free address", func(e *env) { e.created(t, "other"); e.net.LimitAddresses(1) }, func(e *env) { e.net.LimitAddresses(0) }, machine.ErrPreconditionFailed},
		{"the root disk cannot be made", func(e *env) { e.disks.FailAt("create-root-from", errBoom) }, func(e *env) { e.disks.Heal("create-root-from") }, machine.ErrFailed},
		{"the second disk cannot be given", func(e *env) { e.disks.FailAfter("give", e.disks.Calls("give")+1, errBoom) }, func(e *env) { e.disks.Heal("give") }, machine.ErrFailed},
		{"the Driver cannot load the snapshot", func(e *env) { e.driver.FailAt("restore", errBoom) }, func(e *env) { e.driver.Heal("restore") }, machine.ErrFailed},
		{"the Machine cannot be recorded", func(e *env) { e.store.FailAt("save", errBoom) }, func(e *env) { e.store.Heal("save") }, machine.ErrFailed},
	}
	for _, tt := range steps {
		t.Run(tt.name, func(t *testing.T) {
			e := snapshottedWithDisks(t)
			uids := machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256})
			e.withUIDs(t, uids, &fenceBook{fenced: map[machine.MachineID]bool{}})
			tt.fail(e)
			baseline := uids.Held()

			requireKind(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy), tt.kind)

			requireNothingTaken(t, e, uids, baseline, "new", "d1", "d2")

			tt.heal(e)
			require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new2", "snap", narrowPolicy), "control: once the step works the restore makes the Machine")
			assert.Equal(t, baseline+1, uids.Held())
		})
	}
}
