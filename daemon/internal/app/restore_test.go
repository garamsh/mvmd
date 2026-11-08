package app_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/app"
	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha1"
)

func provide(a *app.App) {
	a.Substitutes.Images.Provide(digest)
	a.Substitutes.Disks.Provide("data")
}

// populate creates "a" (holding the disk "data") and "b", arms the deadline and
// starts "a".
func populate(t *testing.T, n *node) {
	t.Helper()
	ctx := context.Background()
	provide(n.app)
	withDisk := create("a")
	withDisk.Disks = []*v1alpha1.DiskAttachment{{DiskId: "data"}}
	_, err := n.client.CreateMachine(ctx, withDisk)
	require.NoError(t, err)
	_, err = n.client.CreateMachine(ctx, create("b"))
	require.NoError(t, err)
	renew(t, n, 1)
	_, err = n.client.StartMachine(ctx, &v1alpha1.StartMachineRequest{MachineId: "a"})
	require.NoError(t, err)
}

// superviseA is what the node holds when "a"'s supervisor survived the
// daemon's restart.
func superviseA(a *app.App) {
	provide(a)
	err := a.Substitutes.Supervisors.Start(context.Background(), "a", machine.ProcessDescription{}, 0)
	if err != nil {
		panic(err)
	}
}

// requireKept requires everything a restart must not lose, with "a" Running.
func requireKept(t *testing.T, n *node) {
	t.Helper()
	ctx := context.Background()

	a, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, a.GetMachine().GetState())
	b, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "b"})
	require.NoError(t, err)
	assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_CREATED, b.GetMachine().GetState())
	list, err := n.client.ListMachines(ctx, &v1alpha1.ListMachinesRequest{})
	require.NoError(t, err)
	require.Len(t, list.GetMachines(), 2)

	// The request-ID memory: the original request returns the original
	// outcome, and another one under the same identifier conflicts.
	again := create("b")
	_, err = n.client.CreateMachine(ctx, again)
	require.NoError(t, err)
	other := create("b")
	other.RequestId = "another"
	_, err = n.client.CreateMachine(ctx, other)
	requireKind(t, err, conflict)

	// The disk-holder index: "a" holds "data", so "b" cannot have it.
	_, err = n.client.AttachDisk(ctx, &v1alpha1.AttachDiskRequest{MachineId: "b", Disk: &v1alpha1.DiskAttachment{DiskId: "data"}})
	requireKind(t, err, precondition)
}

func TestRestore_KeepsWhatTheStateDirectoryHolds(t *testing.T) {
	t.Run("same process", func(t *testing.T) {
		n := start(t, testConfig(t))
		populate(t, n)
		requireKept(t, n)
	})
	t.Run("after a restart, supervisor alive", func(t *testing.T) {
		cfg := testConfig(t)
		first := start(t, cfg)
		populate(t, first)
		first.shutdown(t)

		second := startWith(t, cfg, superviseA)
		requireKept(t, second)
	})
}

func TestRestore_AMachineWithoutASupervisorIsStopped(t *testing.T) {
	ctx := context.Background()
	t.Run("same process", func(t *testing.T) {
		n := start(t, testConfig(t))
		populate(t, n)
		a, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, a.GetMachine().GetState())
		assert.Empty(t, a.GetMachine().GetLastFailureReason())
	})
	t.Run("after a restart, no supervisor", func(t *testing.T) {
		cfg := testConfig(t)
		first := start(t, cfg)
		populate(t, first)
		first.shutdown(t)

		// The tap was applied by the earlier run; the new node still has it.
		second := startWith(t, cfg, func(a *app.App) {
			provide(a)
			_, err := a.Substitutes.Network.Apply(ctx, "a", machine.NetworkPolicy{})
			require.NoError(t, err)
		})
		a, err := second.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_STOPPED, a.GetMachine().GetState())
		assert.Equal(t, "the VMM ended while mvmd was not running", a.GetMachine().GetLastFailureReason())
		_, tap := second.app.Substitutes.Network.Applied("a")
		assert.False(t, tap, "the network of an ended VMM is withdrawn")

		b, err := second.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "b"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_CREATED, b.GetMachine().GetState(), "a Machine that was not running is left as it was")

		// The stopped state is durable: it is what the next restart loads.
		second.shutdown(t)
		third := start(t, cfg)
		a, err = third.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_STOPPED, a.GetMachine().GetState())
	})
}

func TestRestore_FaultsWhatItCannotEstablish(t *testing.T) {
	ctx := context.Background()

	t.Run("the supervisors cannot be listed", func(t *testing.T) {
		cfg := testConfig(t)
		db, err := boltstore.Open(cfg.StateDir)
		require.NoError(t, err)
		running := machine.Machine{
			ID: "a", State: machine.StateRunning,
			Spec: machine.Spec{Image: digest, Resources: machine.Resources{VCPUs: 1, MemoryBytes: 1 << 30}, RootDiskBytes: 1 << 30},
			User: machine.User{UID: 1000, GID: 1000},
		}
		require.NoError(t, db.Machines().Save(ctx, machine.Record{ID: "a", Machine: &running, Request: machine.Request{ID: "req-a", Spec: running.Spec}}))
		require.NoError(t, db.Close())

		n := startWith(t, cfg, func(a *app.App) {
			a.Substitutes.Supervisors.FailAt("live", errors.New("supervisor socket directory unreadable"))
		})
		a, err := n.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_FAULTED, a.GetMachine().GetState())
		assert.Contains(t, a.GetMachine().GetLastFailureReason(), "supervisor socket directory unreadable")
	})

	t.Run("a record cannot be decoded", func(t *testing.T) {
		cfg := testConfig(t)
		first := start(t, cfg)
		populate(t, first)
		first.shutdown(t)

		raw, err := bolt.Open(filepath.Join(cfg.StateDir, "mvmd.db"), 0o600, &bolt.Options{Timeout: time.Second})
		require.NoError(t, err)
		require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
			return tx.Bucket([]byte("machines")).Put([]byte("b"), []byte("{not json"))
		}))
		require.NoError(t, raw.Close())

		second := startWith(t, cfg, superviseA)
		b, err := second.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "b"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_FAULTED, b.GetMachine().GetState())
		assert.Contains(t, b.GetMachine().GetLastFailureReason(), "could not be read")
		a, err := second.client.InspectMachine(ctx, &v1alpha1.InspectMachineRequest{MachineId: "a"})
		require.NoError(t, err)
		assert.Equal(t, v1alpha1.MachineState_MACHINE_STATE_RUNNING, a.GetMachine().GetState(), "one bad record does not touch the others")
	})
}
