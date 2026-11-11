package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

func TestUIDAllocation_DifferentMachinesGetDifferentUIDs(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.created(t, "a")
	e.created(t, "b")

	mA, err := e.svc.Inspect(ctx, "a")
	require.NoError(t, err)
	mB, err := e.svc.Inspect(ctx, "b")
	require.NoError(t, err)
	assert.NotEqual(t, mA.User.UID, mB.User.UID, "two Machines get different UIDs")
	assert.Equal(t, mA.User.UID, mA.User.GID, "UID equals GID for %q", mA.ID)
	assert.Equal(t, mB.User.UID, mB.User.GID, "UID equals GID for %q", mB.ID)
}

func TestUIDAllocation_ReleasedUIDIsReused(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.created(t, "a")
	e.requireState(t, "a", machine.StateCreated)

	uidA, err := e.svc.Inspect(ctx, "a")
	require.NoError(t, err)

	require.NoError(t, e.svc.Delete(ctx, "a"))

	// Now create a new Machine: it should get the same UID.
	e.created(t, "b")
	mB, err := e.svc.Inspect(ctx, "b")
	require.NoError(t, err)
	assert.Equal(t, uidA.User.UID, mB.User.UID, "the released UID is reused")
}

func TestUIDAllocation_ExhaustionIsRefused(t *testing.T) {
	// Use a small range of 3 UIDs.
	env := newEnvWithRange(machine.UIDRange{First: 2000, Count: 3})
	ctx := context.Background()
	e := env

	e.created(t, "a")
	e.created(t, "b")
	e.created(t, "c")
	e.requireState(t, "c", machine.StateCreated)

	// The fourth Machine should fail with ErrPreconditionFailed.
	err := e.svc.Create(ctx, "d", request("d"), baseSpec())
	requireKind(t, err, machine.ErrPreconditionFailed)
}

func TestUIDAllocation_FailedCreateLeavesUIDFree(t *testing.T) {
	ctx := context.Background()
	// Use a range of 2 UIDs. "a" gets 3000, "b" tries and fails, "a" is
	// deleted, "c" should get 3000 again.
	e := newEnvWithRange(machine.UIDRange{First: 3000, Count: 2})

	// First Machine succeeds.
	e.created(t, "a")
	uidA, err := e.svc.Inspect(ctx, "a")
	require.NoError(t, err)

	// Delete it to free the UID.
	require.NoError(t, e.svc.Delete(ctx, "a"))

	// Create a second Machine and inject a failure during Create
	// (image absent triggers a Create failure after UID allocation).
	env := newEnvWithRange(machine.UIDRange{First: 3000, Count: 2})
	e = env

	// Create the second Machine with an absent image to trigger failure
	err = e.svc.Create(ctx, "b", request("b"), machine.Spec{
		Image:         "sha256:other",
		Resources:     machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20},
		RootDiskBytes: 1 << 30,
		Policy:        machine.NetworkPolicy{},
	})
	requireKind(t, err, machine.ErrNotFound)

	// Now the first Machine's UID should be available again.
	// Create a Machine with a valid image — it should succeed and
	// get the first UID (which was previously held by "a").
	e.images.Provide(image)
	e.created(t, "c")
	mC, err := e.svc.Inspect(ctx, "c")
	require.NoError(t, err)
	// "a" got uid 3000, "b" tried to get 3000 and failed, then
	// "a" was deleted, so "c" should get 3000 again.
	assert.Equal(t, uidA.User.UID, mC.User.UID, "the failed Create left the UID free")
}

func TestUIDAllocation_MockDiskRecordsOwner(t *testing.T) {
	e := newEnv()
	e.created(t, "m")

	// Check the in-memory disk recorded the owner.
	diskID := machine.DiskID("m-root")
	owner, ok := e.disks.Owner(diskID)
	require.True(t, ok, "the substitute Disk records the owner")
	assert.Equal(t, machine.MachineID("m"), owner.ID)
	assert.NotZero(t, owner.UID, "the UID is non-zero")
	assert.Equal(t, owner.UID, owner.GID, "UID equals GID")
}

// TestUIDAllocation_StoreSaveFailsLeavesUIDFree ensures that when the state
// store fails to save the record, the UID is released so a subsequent Create
// with a range of one succeeds.
func TestUIDAllocation_StoreSaveFailsLeavesUIDFree(t *testing.T) {
	ctx := context.Background()

	// --- Control: range of one, create succeeds (no failure injected). ---
	ctrl := newEnvWithRange(machine.UIDRange{First: 4000, Count: 1})
	ctrl.created(t, "a")
	requireKind(t, ctrl.svc.Create(ctx, "b", request("b"), baseSpec()), machine.ErrPreconditionFailed)

	// --- Real: store.Save fails. The leaked UID is released, so a new
	// Create with range of one should now succeed. ---
	real := newEnvWithRange(machine.UIDRange{First: 4000, Count: 1})
	real.store.FailAt("save", errBoom)
	err := real.svc.Create(ctx, "x", request("x"), baseSpec())
	require.Error(t, err, "save failure should abort Create")

	// The UID is released, so a new Create succeeds.
	real.store.Heal("save")
	real.created(t, "y")
	require.NoError(t, real.svc.Delete(ctx, "y"))
}

// TestUIDRange_Validate checks that the range refuses root, zero-count, and
// overflow, beside valid near-misses.
func TestUIDRange_Validate(t *testing.T) {
	tests := []struct {
		name    string
		r       machine.UIDRange
		wantErr bool
	}{
		{"root is refused", machine.UIDRange{First: 0, Count: 1}, true},
		{"zero count is refused", machine.UIDRange{First: 1, Count: 0}, true},
		{"overflow is refused", machine.UIDRange{First: ^uint32(0), Count: 2}, true},
		{"exact max does not wrap", machine.UIDRange{First: 1, Count: ^uint32(0)}, false},
		{"smallest valid range", machine.UIDRange{First: 1, Count: 1}, false},
		{"highest single-entry range", machine.UIDRange{First: ^uint32(0), Count: 1}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.r.Validate()
			if tt.wantErr {
				require.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// newEnvWithRange is like newEnv but with a custom UID range.
func newEnvWithRange(uidRange machine.UIDRange) *env {
	return newEnvWithRangeAndCaps(uidRange, hotplugAndResize...)
}

// newEnvWithRangeAndCaps is newEnvWithRange with the Driver's capabilities.
func newEnvWithRangeAndCaps(uidRange machine.UIDRange, caps ...machine.Capability) *env {
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: caps}),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		snaps:  memory.NewSnapshotStore(),
		guest:  memory.NewGuest(0),
		clock:  memory.NewWallClock(epoch),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
		events: memory.NewEvents(),
	}
	e.images.Provide(image)
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(uidRange), e.events, machine.WithKernels(memory.Kernels()))
	return e
}
