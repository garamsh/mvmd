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

// env is a service over the in-memory substitutes, which the test keeps hold
// of to configure and to read back.
type env struct {
	svc    machine.Service
	driver *memory.Driver
	sup    *memory.Supervision
	net    *memory.Network
	disks  *memory.Disk
	images *memory.ImageStore
	store  *memory.StateStore
	permit *memory.StartPermitter
}

// newEnv returns an env whose Driver supports the given capabilities and
// whose node holds the image.
func newEnv(caps ...machine.Capability) *env {
	e := &env{
		driver: memory.NewDriver(machine.DriverInfo{Name: "inmem", Version: "1", Capabilities: caps}),
		sup:    memory.NewSupervision(),
		net:    memory.NewNetwork(),
		disks:  memory.NewDisk(),
		images: memory.NewImageStore(),
		store:  memory.NewStateStore(),
		permit: memory.NewStartPermitter(),
	}
	e.images.Provide(image)
	e.svc = machine.NewService(e.driver, e.sup, e.net, e.disks, e.images, e.store, e.permit)
	return e
}

var hotplugAndResize = []machine.Capability{machine.CapabilityDiskHotplug, machine.CapabilityResize}

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
	require.NoError(t, e.svc.Create(context.Background(), id, request(id), baseSpec()))
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
	e := newEnv()
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
