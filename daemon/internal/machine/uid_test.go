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

// newEnvWithRange is like newEnv but with a custom UID range.
func newEnvWithRange(uidRange machine.UIDRange) *env {
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", Capabilities: hotplugAndResize}),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
	}
	e.images.Provide(image)
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.store, e.permit, uidRange)
	return e
}
