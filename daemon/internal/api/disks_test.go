package api_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// diskKeeper is a machine.Service whose disk calls answer fixed values and
// record what they were asked.
type diskKeeper struct {
	machine.Service
	listed  []machine.DiskInfo
	err     error
	created []machine.DiskEntry
	deleted []machine.DiskID
}

func (d *diskKeeper) CreateDisk(_ context.Context, disk machine.DiskID, request machine.RequestID, size uint64) error {
	d.created = append(d.created, machine.DiskEntry{ID: disk, Request: request, SizeBytes: size})
	return d.err
}

func (d *diskKeeper) ListDisks(context.Context) ([]machine.DiskInfo, error) { return d.listed, d.err }

func (d *diskKeeper) DeleteDisk(_ context.Context, disk machine.DiskID) error {
	d.deleted = append(d.deleted, disk)
	return d.err
}

func TestCreateDisk_PassesTheRequestAndReportsTheDisk(t *testing.T) {
	m := &diskKeeper{}

	resp, err := api.New(m, nil, nil, nil, nil).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "req", SizeBytes: 1 << 20})

	require.NoError(t, err)
	assert.Equal(t, []machine.DiskEntry{{ID: "data", Request: "req", SizeBytes: 1 << 20}}, m.created)
	assert.Equal(t, "data", resp.GetDisk().GetId())
	assert.Equal(t, uint64(1<<20), resp.GetDisk().GetSizeBytes())
}

func TestCreateDisk_RejectsAZeroSizeBeforeTheUseCase(t *testing.T) {
	m := &diskKeeper{}

	_, err := api.New(m, nil, nil, nil, nil).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "req"})

	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.Empty(t, m.created, "the use case was not asked")
}

func TestCreateDisk_MapsTheUseCaseOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
	}{
		{"conflict", machine.NewError(machine.ErrConflict, "other request"), v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT},
		{"failure", machine.NewError(machine.ErrFailed, "boom"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &diskKeeper{err: tt.err}

			resp, err := api.New(m, nil, nil, nil, nil).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "req", SizeBytes: 1})

			assert.Nil(t, resp)
			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
		})
	}
}

func TestListDisks_ReportsEveryDiskWithItsHolder(t *testing.T) {
	m := &diskKeeper{listed: []machine.DiskInfo{{ID: "a", SizeBytes: 1 << 20}, {ID: "b", SizeBytes: 2 << 20, Holder: "m"}}}

	resp, err := api.New(m, nil, nil, nil, nil).ListDisks(context.Background(), &v1alpha2.ListDisksRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Disks, 2)
	assert.Equal(t, "a", resp.Disks[0].Id)
	assert.Equal(t, uint64(1<<20), resp.Disks[0].SizeBytes)
	assert.Empty(t, resp.Disks[0].HolderMachineId)
	assert.Equal(t, "b", resp.Disks[1].Id)
	assert.Equal(t, "m", resp.Disks[1].HolderMachineId)
}

func TestListDisks_MapsTheUseCaseOutcome(t *testing.T) {
	m := &diskKeeper{err: machine.NewError(machine.ErrFailed, "list: boom")}

	resp, err := api.New(m, nil, nil, nil, nil).ListDisks(context.Background(), &v1alpha2.ListDisksRequest{})

	assert.Nil(t, resp)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, v1alpha2.KindOf(err))
}

func TestDeleteDisk_PassesTheIdentifierAndMapsTheUseCaseOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
	}{
		{"success", nil, v1alpha2.OutcomeKind_OUTCOME_KIND_SUCCESS},
		{"held", machine.NewError(machine.ErrPreconditionFailed, "held"), v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED},
		{"failure", machine.NewError(machine.ErrFailed, "remove: boom"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &diskKeeper{err: tt.err}

			_, err := api.New(m, nil, nil, nil, nil).DeleteDisk(context.Background(), &v1alpha2.DeleteDiskRequest{DiskId: "data"})

			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
			assert.Equal(t, []machine.DiskID{"data"}, m.deleted)
		})
	}
}

func TestDeleteDisk_RejectsAnEmptyIdentifierBeforeTheUseCase(t *testing.T) {
	m := &diskKeeper{}

	_, err := api.New(m, nil, nil, nil, nil).DeleteDisk(context.Background(), &v1alpha2.DeleteDiskRequest{})

	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.Empty(t, m.deleted)
}
