package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// TestRestore_RebuiltUsedUIDAllocatesADifferentUID creates a Machine (UID 1000),
// reopens the service through Restore, then creates a second Machine.
// The reopened service must rebuild usedUID so the second Machine gets a
// different UID than the first. The control is the same sequence without
// reopening, which also gives different UIDs.
func TestRestore_RebuiltUsedUIDAllocatesADifferentUID(t *testing.T) {
	ctx := context.Background()
	e := newEnv(hotplugAndResize...)
	e.created(t, "a")

	uidA, err := e.svc.Inspect(ctx, "a")
	require.NoError(t, err)

	// Reopen: the service is rebuilt from the store, as if the daemon
	// restarted.
	svc := e.reopened(t)

	// Create a second Machine: it must not receive the same UID as a.
	err = svc.Create(ctx, "b", request("b"), baseSpec())
	require.NoError(t, err)

	uidB, err := svc.Inspect(ctx, "b")
	require.NoError(t, err)
	assert.NotEqual(t, uidA.User.UID, uidB.User.UID,
		"after Restore, a new Machine gets a different UID than the restored one")
}

// TestRestore_UIDOutsideRangeIsRejected verifies that a restored record
// carrying a UID outside the configured range causes Restore to fail.
func TestRestore_UIDOutsideRangeIsRejected(t *testing.T) {
	// Build a service with a small range.
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", Capabilities: hotplugAndResize}),
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
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 2000, Count: 10}), e.events)

	// Save a record with UID 9000 — outside the range.
	m := machine.Machine{
		ID:    "rogue",
		State: machine.StateCreated,
		Spec:  baseSpec(),
		User:  machine.User{UID: 9000, GID: 9000},
	}
	require.NoError(t, e.store.Save(context.Background(), machine.Record{
		ID:      "rogue",
		Machine: &m,
		Request: machine.Request{ID: "req-rogue", Spec: baseSpec()},
	}))

	err := e.svc.Restore(context.Background())
	require.Error(t, err, "Restore must reject a UID outside the configured range")
}
