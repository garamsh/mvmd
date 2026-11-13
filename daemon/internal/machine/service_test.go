package machine_test

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
)

const image machine.ImageDigest = "sha256:image"

var errBoom = errors.New("injected failure")

// epoch is the host's wall clock in a test until it is moved.
var epoch = time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)

// env is a service over the in-memory substitutes, which the test keeps hold
// of to configure and to read back.
type env struct {
	svc    machine.Service
	driver *memory.Driver
	sup    *memory.Supervision
	net    *memory.Network
	disks  *memory.Disk
	images *memory.ImageStore
	snaps  *memory.SnapshotStore
	// dsnaps are the disk snapshots the node holds.
	dsnaps *memory.DiskSnapshotStore
	guest  *memory.Guest
	clock  *memory.WallClock
	store  *memory.StateStore
	permit *memory.StartPermitter
	events *memory.Events
	// ceiling is the ceiling created gives a Machine, none by default.
	ceiling machine.Resources
}

// newEnv returns an env whose Driver supports the given capabilities and
// whose node holds the image.
func newEnv(caps ...machine.Capability) *env {
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu-a", Capabilities: caps}),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		snaps:  memory.NewSnapshotStore(),
		dsnaps: memory.NewDiskSnapshotStore(),
		guest:  memory.NewGuest(0),
		clock:  memory.NewWallClock(epoch),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
		events: memory.NewEvents(),
	}
	e.images.Provide(image)
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.snaps, e.guest, e.clock, e.store, e.permit, machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), e.events, machine.WithKernels(memory.Kernels()), machine.WithDiskSnapshots(e.dsnaps))
	return e
}

var hotplugAndResize = []machine.Capability{machine.CapabilityDiskHotplug, machine.CapabilityCPUResize, machine.CapabilityMemoryResize}

func baseSpec() machine.Spec {
	return machine.Spec{
		Image:         image,
		Resources:     machine.Resources{VCPUs: 2, MemoryBytes: 512 << 20},
		RootDiskBytes: 1 << 30,
		Policy: machine.NetworkPolicy{Egress: []machine.NetworkRule{{
			Prefix:   netip.MustParsePrefix("10.1.0.0/16"),
			Protocol: machine.ProtocolTCP,
			Ports:    []machine.PortRange{{First: 443, Last: 443}},
		}}},
	}
}

func request(id machine.MachineID) machine.RequestID { return machine.RequestID("req-" + id) }

// created creates the Machine with the base spec.
func (e *env) created(t *testing.T, id machine.MachineID) {
	t.Helper()
	spec := baseSpec()
	spec.Ceiling = e.ceiling
	require.NoError(t, e.svc.Create(context.Background(), id, request(id), spec))
	e.requireState(t, id, machine.StateCreated)
}

// inState creates the Machine and takes it to the state through the service.
func (e *env) inState(t *testing.T, id machine.MachineID, state machine.State) {
	t.Helper()
	ctx := context.Background()
	e.created(t, id)
	switch state {
	case machine.StateCreated:
	case machine.StateRunning:
		require.NoError(t, e.svc.Start(ctx, id))
	case machine.StatePaused:
		require.NoError(t, e.svc.Start(ctx, id))
		require.NoError(t, e.svc.Pause(ctx, id))
	case machine.StateStopped:
		require.NoError(t, e.svc.Start(ctx, id))
		require.NoError(t, e.svc.Stop(ctx, id, time.Second))
	case machine.StateFaulted:
		require.NoError(t, e.svc.Start(ctx, id))
		e.sup.FailAt("terminate", errBoom)
		require.ErrorIs(t, e.svc.Stop(ctx, id, time.Second), machine.ErrFailed)
		e.sup.Heal("terminate")
	}
	e.requireState(t, id, state)
}

func (e *env) requireState(t *testing.T, id machine.MachineID, want machine.State) machine.Machine {
	t.Helper()
	m, err := e.svc.Inspect(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, want, m.State, "state of %q", id)
	return m
}

// requireStored reads back what the State Store holds for the Machine.
func (e *env) requireStored(t *testing.T, id machine.MachineID, want machine.State) {
	t.Helper()
	rec, ok := e.store.Record(id)
	require.True(t, ok, "no record of %q", id)
	require.NotNil(t, rec.Machine)
	require.Equal(t, want, rec.Machine.State, "recorded state of %q", id)
}

func (e *env) requireNoMachine(t *testing.T, id machine.MachineID) {
	t.Helper()
	_, err := e.svc.Inspect(context.Background(), id)
	requireKind(t, err, machine.ErrNotFound)
	_, held := e.disks.Holds(machine.DiskID(string(id) + "-root"))
	assert.False(t, held, "root disk of %q exists", id)
	assert.False(t, e.net.Allocated(id), "addresses of %q held", id)
	rec, ok := e.store.Record(id)
	assert.False(t, ok && rec.Machine != nil, "a Machine %q is recorded", id)
}

// requireKind requires err to be an outcome of the kind, carrying a reason.
func requireKind(t *testing.T, err error, kind error) {
	t.Helper()
	require.ErrorIs(t, err, kind)
	var outcome *machine.Error
	require.ErrorAs(t, err, &outcome)
	assert.NotEmpty(t, outcome.Reason, "an outcome carries its reason")
}

func TestService_ConcurrentCallsAreSerialized(t *testing.T) {
	e := newEnv(machine.CapabilityDiskHotplug)
	ctx := context.Background()
	e.disks.Provide("shared")
	for _, id := range []machine.MachineID{"a", "b"} {
		e.created(t, id)
	}

	var wg sync.WaitGroup
	attached := make(chan machine.MachineID, 40)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := machine.MachineID([]string{"a", "b"}[i%2])
			if e.svc.AttachDisk(ctx, id, machine.DiskAttachment{Disk: "shared"}) == nil {
				attached <- id
			}
			other := machine.MachineID(fmt.Sprintf("m%d", i))
			_ = e.svc.Create(ctx, other, request(other), baseSpec())
			_, _ = e.svc.List(ctx)
		}()
	}
	wg.Wait()
	close(attached)

	holders := map[machine.MachineID]bool{}
	for id := range attached {
		holders[id] = true
	}
	// A Machine that already holds the disk may succeed again (the repeat
	// rule), but the other Machine never does.
	assert.Len(t, holders, 1, "exactly one Machine holds the shared disk")
	all, err := e.svc.List(ctx)
	require.NoError(t, err)
	assert.Len(t, all, 42)
}

// A Stop waiting out a long grace period holds only its own Machine.
func TestService_LongStopGraceDoesNotBlockOtherMachines(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	e.inState(t, "a", machine.StateRunning)
	e.created(t, "b")
	entered, release := e.driver.HoldShutdown()

	stopped := make(chan error, 1)
	go func() { stopped <- e.svc.Stop(ctx, "a", time.Hour) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop never reached the guest")
	}

	others := make(chan error, 1)
	go func() {
		if _, err := e.svc.Inspect(ctx, "b"); err != nil {
			others <- err
			return
		}
		if _, err := e.svc.Inspect(ctx, "a"); err != nil {
			others <- err
			return
		}
		if _, err := e.svc.List(ctx); err != nil {
			others <- err
			return
		}
		if err := e.svc.Start(ctx, "b"); err != nil {
			others <- err
			return
		}
		others <- e.svc.Create(ctx, "c", "r", baseSpec())
	}()
	select {
	case err := <-others:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("calls on other Machines waited for the stop's grace period")
	}
	e.requireState(t, "b", machine.StateRunning)
	e.requireState(t, "a", machine.StateRunning) // still stopping

	release()
	require.NoError(t, <-stopped)
	e.requireState(t, "a", machine.StateStopped)
}

// The per-Machine locks exist only while a call uses them, so neither Machine
// churn nor calls naming unknown identifiers grow them.
func TestService_LocksReturnToEmpty(t *testing.T) {
	e := newEnv()
	ctx := context.Background()
	assert.Zero(t, machine.LockCount(e.svc))

	e.created(t, "m")
	require.NoError(t, e.svc.Delete(ctx, "m"))
	assert.Zero(t, machine.LockCount(e.svc), "after create and delete")

	for i := range 50 {
		id := machine.MachineID(fmt.Sprintf("unknown-%d", i))
		requireKind(t, e.svc.Start(ctx, id), machine.ErrNotFound)
		requireKind(t, e.svc.Stop(ctx, id, time.Second), machine.ErrNotFound)
		require.NoError(t, e.svc.Delete(ctx, id))
	}
	assert.Zero(t, machine.LockCount(e.svc), "after calls naming unknown identifiers")

	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := machine.MachineID(fmt.Sprintf("c%d", i%4))
			_ = e.svc.Create(ctx, id, request(id), baseSpec())
			_ = e.svc.Delete(ctx, id)
		}()
	}
	wg.Wait()
	assert.Zero(t, machine.LockCount(e.svc), "after concurrent churn")
}
