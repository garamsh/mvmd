package machine_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

// TestRestore_MaxRangeDoesNotWrap verifies that a range of
// {First: 1, Count: MaxUint32} — which Validate accepts — restores
// a Machine whose UID is 1 without wrapping the end bound to zero.
func TestRestore_MaxRangeDoesNotWrap(t *testing.T) {
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", Capabilities: hotplugAndResize}),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		snaps:  memory.NewSnapshotStore(),
		guest:  memory.NewGuest(0),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
		events: memory.NewEvents(),
	}
	e.images.Provide(image)
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.store, e.permit, machine.UIDRange{First: 1, Count: ^uint32(0)}, e.events)

	// Create a Machine that gets UID 1.
	e.created(t, "a")

	// Save the record (already saved by created).
	// Reopen: Restore must accept UID 1, not reject it as outside [1, 0).
	svc := e.reopenedWithRange(t, machine.UIDRange{First: 1, Count: ^uint32(0)})

	m, err := svc.Inspect(context.Background(), "a")
	require.NoError(t, err)
	require.Equal(t, machine.User{UID: 1, GID: 1}, m.User,
		"UID 1 is in range [1, 0xFFFFFFFF] and restores without wrap")
}

// TestRestore_UIDZeroIsRefused verifies that a restored record carrying
// UID 0 is rejected by Restore, not silently skipped. A record with
// UID 0 would give root to the VMM, violating ADR 0015.
func TestRestore_UIDZeroIsRefused(t *testing.T) {
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", Capabilities: hotplugAndResize}),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		snaps:  memory.NewSnapshotStore(),
		guest:  memory.NewGuest(0),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
		events: memory.NewEvents(),
	}
	e.images.Provide(image)
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.store, e.permit, machine.UIDRange{First: 1000, Count: 10}, e.events)

	// Save a record with UID 0.
	m := machine.Machine{
		ID:    "root-machine",
		State: machine.StateCreated,
		Spec:  baseSpec(),
		User:  machine.User{UID: 0, GID: 0},
	}
	require.NoError(t, e.store.Save(context.Background(), machine.Record{
		ID:      "root-machine",
		Machine: &m,
		Request: machine.Request{ID: "req-root", Spec: baseSpec()},
	}))

	err := e.svc.Restore(context.Background())
	require.Error(t, err, "Restore must refuse UID 0")
	require.Contains(t, err.Error(), "UID 0")
}

// TestRestore_UIDCollisionIsRefused verifies that two records carrying
// the same UID cause Restore to fail.
func TestRestore_UIDCollisionIsRefused(t *testing.T) {
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", Capabilities: hotplugAndResize}),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		snaps:  memory.NewSnapshotStore(),
		guest:  memory.NewGuest(0),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
		events: memory.NewEvents(),
	}
	e.images.Provide(image)
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.store, e.permit, machine.UIDRange{First: 2000, Count: 100}, e.events)

	// Save two records with the same UID.
	for _, id := range []string{"a", "b"} {
		m := machine.Machine{
			ID:    machine.MachineID(id),
			State: machine.StateCreated,
			Spec:  baseSpec(),
			User:  machine.User{UID: 2005, GID: 2005},
		}
		require.NoError(t, e.store.Save(context.Background(), machine.Record{
			ID:      machine.MachineID(id),
			Machine: &m,
			Request: machine.Request{ID: machine.RequestID("req-" + id), Spec: baseSpec()},
		}))
	}

	err := e.svc.Restore(context.Background())
	require.Error(t, err, "Restore must refuse two records sharing a UID")
	require.Contains(t, err.Error(), "share UID")
}

// reopenedWithRange is like reopened but with a custom UID range.
func (e *env) reopenedWithRange(t *testing.T, uidRange machine.UIDRange) machine.Service {
	t.Helper()
	svc := machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.store, e.permit, uidRange, e.events)
	require.NoError(t, svc.Restore(context.Background()))
	return svc
}
