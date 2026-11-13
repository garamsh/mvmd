package boltstore_test

import (
	"context"
	"net/netip"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"

	"github.com/garamsh/mvmd/daemon/internal/boltstore"
	"github.com/garamsh/mvmd/daemon/internal/fencing"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

func spec() machine.Spec {
	return machine.Spec{
		Image:         "sha256:image",
		Resources:     machine.Resources{VCPUs: 2, MemoryBytes: 1 << 30},
		Ceiling:       machine.Resources{VCPUs: 4, MemoryBytes: 2 << 30},
		RootDiskBytes: 1 << 30,
		Disks:         []machine.DiskAttachment{{Disk: "d", ReadOnly: true, MountPath: "/data"}},
		Policy: machine.NetworkPolicy{
			Egress: []machine.NetworkRule{{
				Prefix:   netip.MustParsePrefix("10.0.0.0/8"),
				Protocol: machine.ProtocolTCP,
				Ports:    []machine.PortRange{{First: 80, Last: 443}},
			}},
			Ingress: []machine.NetworkRule{{Prefix: netip.MustParsePrefix("fd00::/8"), Protocol: machine.ProtocolICMPv6}},
		},
	}
}

func records() []machine.Record {
	m := machine.Machine{
		ID:            "m",
		State:         machine.StateStopped,
		Spec:          spec(),
		RootDisk:      "m-root",
		Network:       machine.NetworkIdentity{MAC: "02:00:00:00:00:01", Addresses: []netip.Addr{netip.MustParseAddr("10.0.0.1")}},
		FailureReason: "boot failed: boom",
		// Differs from the resources, so a reading that defaulted it would show.
		BootMemoryBytes: 512 << 20,
	}
	return []machine.Record{
		{ID: "m", Machine: &m, Request: machine.Request{ID: "r1", Snapshot: "snap-1", Spec: spec()}},
		// A failed creating call: no Machine, and the original outcome.
		{ID: "failed", Request: machine.Request{ID: "r2", Spec: spec(), Failure: machine.NewError(machine.ErrFailed, "create root disk: boom")}},
	}
}

// Every record type is read back after the database is closed and reopened,
// which is the durability a clean close and reopen can show.
func TestStore_RecordsSurviveCloseAndReopen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := boltstore.Open(dir)
	require.NoError(t, err)
	for _, r := range records() {
		require.NoError(t, db.Machines().Save(ctx, r))
	}
	deadline := fencing.Record{Expiry: fencing.BootTime(90 * time.Minute), BootID: "boot-1", Length: time.Minute, Renewal: 42}
	require.NoError(t, db.Deadline().Save(ctx, deadline))
	require.NoError(t, db.Close())

	db, err = boltstore.Open(dir)
	require.NoError(t, err)
	defer db.Close()

	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	want := records()
	require.Len(t, got, 2)
	// Ordered by identifier: "failed" before "m".
	assert.Equal(t, want[1].Request.Failure.Reason, got[0].Request.Failure.Reason)
	assert.ErrorIs(t, got[0].Request.Failure, machine.ErrFailed, "the outcome kind is restored")
	assert.Nil(t, got[0].Machine)
	assert.Equal(t, want[1].Request.Spec, got[0].Request.Spec)
	assert.Equal(t, want[0], got[1])

	loaded, ok, err := db.Deadline().Load(ctx)
	require.NoError(t, err)
	require.True(t, ok)
	assert.Equal(t, deadline, loaded)
}

func TestStore_SaveReplacesAndRemoveDeletes(t *testing.T) {
	ctx := context.Background()
	db, err := boltstore.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()

	r := records()[0]
	require.NoError(t, db.Machines().Save(ctx, r))
	r.Machine.State = machine.StateRunning
	require.NoError(t, db.Machines().Save(ctx, r))
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, machine.StateRunning, got[0].Machine.State)

	require.NoError(t, db.Machines().Remove(ctx, "m"))
	require.NoError(t, db.Machines().Remove(ctx, "m"), "removing an absent record succeeds")
	got, err = db.Machines().Load(ctx)
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestStore_NoDeadlineRecordIsNotAnError(t *testing.T) {
	db, err := boltstore.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	_, ok, err := db.Deadline().Load(context.Background())
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestStore_ACancelledContextWritesNothing(t *testing.T) {
	db, err := boltstore.Open(t.TempDir())
	require.NoError(t, err)
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, db.Machines().Save(ctx, records()[0]), context.Canceled)
	require.ErrorIs(t, db.Deadline().Save(ctx, fencing.Record{}), context.Canceled)
	got, err := db.Machines().Load(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestStore_OneProcessHoldsTheFile(t *testing.T) {
	dir := t.TempDir()
	db, err := boltstore.Open(dir)
	require.NoError(t, err)
	defer db.Close()
	_, err = boltstore.Open(dir)
	require.Error(t, err, "a second open of the same file does not wait for the first")
	assert.FileExists(t, filepath.Join(dir, "mvmd.db"))
}

func TestStore_UndecodableRecordIsReturnedUnreadableBesideTheOthers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := boltstore.Open(dir)
	require.NoError(t, err)
	require.NoError(t, db.Machines().Save(ctx, records()[0]))
	require.NoError(t, db.Close())

	raw, err := bolt.Open(filepath.Join(dir, "mvmd.db"), 0o600, nil)
	require.NoError(t, err)
	require.NoError(t, raw.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("machines")).Put([]byte("a-broken"), []byte("{not json"))
	}))
	require.NoError(t, raw.Close())

	db, err = boltstore.Open(dir)
	require.NoError(t, err)
	defer db.Close()
	got, err := db.Machines().Load(ctx)
	require.NoError(t, err, "one bad record does not fail the load")
	require.Len(t, got, 2)
	assert.Equal(t, machine.MachineID("a-broken"), got[0].ID)
	assert.NotEmpty(t, got[0].Unreadable)
	assert.Nil(t, got[0].Machine)
	assert.Empty(t, got[1].Unreadable)
	assert.Equal(t, records()[0].Machine.ID, got[1].ID)
}

// The adapters satisfy the ports they serve.
var (
	_ machine.StateStore    = (*boltstore.MachineStore)(nil)
	_ fencing.DeadlineStore = (*boltstore.DeadlineStore)(nil)
)
