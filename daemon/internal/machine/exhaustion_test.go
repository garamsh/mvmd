package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// exhaustible is an env whose UID range holds users, and the allocator it
// draws from, so a test reads back what the allocator holds.
func exhaustible(t *testing.T, users uint32) (*env, *machine.UIDs) {
	t.Helper()
	e := newEnv(snapshotDiskCaps...)
	uids := machine.NewUIDs(machine.UIDRange{First: 1000, Count: users})
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, uids, e.events, machine.WithKernels(memory.Kernels()))
	return e, uids
}

// resource is one of the two things whose exhaustion refuses a Create machine
// or a Restore: the UID range and the network's addresses.
type resource struct {
	name string
	// users is the size of the UID range; it leaves a user free where the
	// address is what runs out.
	users uint32
	// spend makes the node hold one Machine and leaves none of the resource.
	spend func(t *testing.T, e *env)
	// reason is what the refusal names.
	reason string
}

var resources = []resource{
	{
		name:  "the UID range",
		users: 1,
		spend: func(t *testing.T, e *env) {
			e.created(t, "other")
		},
		reason: "the UID range is exhausted",
	},
	{
		name:  "the network's addresses",
		users: 2,
		spend: func(t *testing.T, e *env) {
			e.net.LimitAddresses(1)
			e.created(t, "other")
		},
		reason: "the prefix has no free address",
	},
}

// held is what a node holds that a refused call must leave as it found it.
type held struct {
	uids      int
	machines  []machine.MachineID
	holders   map[machine.DiskID]machine.MachineID
	addressed bool
}

func (e *env) holding(t *testing.T, uids *machine.UIDs, refused machine.MachineID, disks ...machine.DiskID) held {
	t.Helper()
	listed, err := e.svc.List(context.Background())
	require.NoError(t, err)
	h := held{uids: uids.Held(), holders: make(map[machine.DiskID]machine.MachineID), addressed: e.net.Allocated(refused)}
	for _, m := range listed {
		h.machines = append(h.machines, m.ID)
	}
	all, err := e.svc.ListDisks(context.Background())
	require.NoError(t, err)
	for _, d := range all {
		h.holders[d.ID] = d.Holder
	}
	for _, d := range disks {
		_, ok := h.holders[d]
		require.True(t, ok, "the setup: disk %q is on the node", d)
	}
	return h
}

func TestCreate_AnExhaustedResourceIsRefusedAsPreconditionFailedWithNothingTaken(t *testing.T) {
	ctx := context.Background()
	for _, r := range resources {
		t.Run(r.name, func(t *testing.T) {
			e, uids := exhaustible(t, r.users)
			e.disks.Provide("d1")
			r.spend(t, e)
			before := e.holding(t, uids, "new", "d1")
			require.Equal(t, 1, before.uids, "the setup: one Machine holds a user")

			spec := baseSpec()
			spec.Disks = []machine.DiskAttachment{rw("d1")}
			err := e.svc.Create(ctx, "new", "req-new", spec)

			requireKind(t, err, machine.ErrPreconditionFailed)
			var outcome *machine.Error
			require.ErrorAs(t, err, &outcome)
			assert.Contains(t, outcome.Reason, r.reason)
			assert.Equal(t, before, e.holding(t, uids, "new", "d1"), "the refused call took no user, address, disk or record")
			e.requireNoMachine(t, "new")

			// The control: the same call, once the one slot is free, succeeds,
			// so the refusal above was the resource and nothing else.
			require.NoError(t, e.svc.Delete(ctx, "other"))
			require.NoError(t, e.svc.Create(ctx, "new", "req-new", spec))
			e.requireState(t, "new", machine.StateCreated)
			assert.Equal(t, 1, uids.Held())
		})
	}
}

func TestRestoreSnapshot_AnExhaustedResourceIsRefusedAsPreconditionFailedWithNothingTaken(t *testing.T) {
	ctx := context.Background()
	for _, r := range resources {
		t.Run(r.name, func(t *testing.T) {
			e, uids := exhaustible(t, r.users)
			snapshottedWithDisksIn(t, e)
			r.spend(t, e)
			before := e.holding(t, uids, "new", "d1", "d2")
			require.Equal(t, 1, before.uids, "the setup: one Machine holds a user")

			err := e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy)

			requireKind(t, err, machine.ErrPreconditionFailed)
			var outcome *machine.Error
			require.ErrorAs(t, err, &outcome)
			assert.Contains(t, outcome.Reason, r.reason)
			assert.Equal(t, before, e.holding(t, uids, "new", "d1", "d2"), "the refused call took no user, address, disk or record")
			e.requireNoMachine(t, "new")
			assert.Zero(t, e.driver.Calls("describe-restore"), "nothing was begun")

			// The control: the same call, once the one slot is free, succeeds.
			require.NoError(t, e.svc.Delete(ctx, "other"))
			require.NoError(t, e.svc.RestoreSnapshot(ctx, "new", "r-new", "snap", narrowPolicy))
			e.requireState(t, "new", machine.StateRunning)
			assert.Equal(t, 1, uids.Held())
		})
	}
}
