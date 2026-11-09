package fencing_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/fencing/memory"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	machinememory "github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

const image machine.ImageDigest = "sha256:image"

var errBoom = errors.New("injected failure")

// env is the deadline over its substitutes, with the lifecycle wired to it as
// the composition site will wire it.
type env struct {
	clock *memory.Clock
	store *memory.DeadlineStore
	sup   *memory.Supervisors
	svc   fencing.Service
	// events records what the deadline reported, across restarts.
	events *memory.Events

	driver *machinememory.Driver
	disks  *machinememory.Disk
	snaps  *machinememory.SnapshotStore
	m      machine.Service
}

// newEnv returns a reconciled env whose deadline is unarmed.
func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{clock: memory.NewClock("boot-1"), store: memory.NewDeadlineStore()}
	e.sup = memory.NewSupervisors(e.store)
	e.events = memory.NewEvents()
	e.svc = fencing.NewService(e.clock, e.store, e.sup, e.events)

	e.driver = machinememory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: []machine.Capability{machine.CapabilitySnapshot}})
	e.disks = machinememory.NewDisk()
	e.snaps = machinememory.NewSnapshotStore()
	images := machinememory.NewImageStore()
	images.Provide(image)
	e.m = machine.NewService(e.driver, e.svc, machinememory.NewNetwork(), e.disks, images, e.snaps, machinememory.NewGuest(0), machinememory.NewWallClock(time.Now()), machinememory.NewStateStore(), e.svc, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), machinememory.NewEvents())
	require.NoError(t, e.svc.Reconcile(context.Background(), e.m))
	return e
}

// restart replaces the fencing service by a new one over the same store,
// supervisors and clock, as an mvmd restart does, and reconciles it.
func (e *env) restart(t *testing.T) error {
	t.Helper()
	e.svc = fencing.NewService(e.clock, e.store, e.sup, e.events)
	return e.svc.Reconcile(context.Background(), e.m)
}

// running creates the Machines and starts them, which needs an armed deadline.
func (e *env) running(t *testing.T, ids ...machine.MachineID) {
	t.Helper()
	for _, id := range ids {
		spec := machine.Spec{
			Image:         image,
			Resources:     machine.Resources{VCPUs: 1, MemoryBytes: 1 << 20},
			RootDiskBytes: 1 << 20,
		}
		require.NoError(t, e.m.Create(context.Background(), id, machine.RequestID("r-"+id), spec))
		require.NoError(t, e.m.Start(context.Background(), id))
	}
}

func (e *env) renew(t *testing.T, length time.Duration, number uint64) fencing.Status {
	t.Helper()
	st, err := e.svc.Renew(context.Background(), length, number)
	require.NoError(t, err)
	return st
}

func (e *env) status(t *testing.T) fencing.Status {
	t.Helper()
	st, err := e.svc.Inspect(context.Background())
	require.NoError(t, err)
	return st
}

func (e *env) state(t *testing.T, id machine.MachineID) machine.State {
	t.Helper()
	m, err := e.m.Inspect(context.Background(), id)
	require.NoError(t, err)
	return m.State
}

// requireSupervisorsAt requires every live supervisor to hold the recorded
// expiry, and no supervisor to have held a later one at any moment.
func (e *env) requireSupervisorsAt(t *testing.T) {
	t.Helper()
	rec, ok := e.store.Recorded()
	require.True(t, ok)
	live, err := e.sup.Live(context.Background())
	require.NoError(t, err)
	for _, sv := range live {
		assert.Equal(t, rec.Expiry, sv.Expiry, "supervisor of %q", sv.Machine)
	}
	assert.Empty(t, e.store.Violations())
}

func requireKind(t *testing.T, err, kind error) {
	t.Helper()
	require.ErrorIs(t, err, kind)
	var outcome *machine.Error
	require.ErrorAs(t, err, &outcome)
	assert.NotEmpty(t, outcome.Reason)
}

// runUntilStopped runs the deadline's expiry loop and returns a function that
// stops it.
func (e *env) run(t *testing.T) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- e.svc.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			assert.ErrorIs(t, err, context.Canceled)
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not end")
		}
	}
}

func (e *env) eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	require.Eventually(t, cond, 5*time.Second, time.Millisecond, what)
}
