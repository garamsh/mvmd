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

// snapshotter is a machine.Service whose snapshot calls answer fixed values
// and record what they were asked.
type snapshotter struct {
	machine.Service
	listed  []machine.Snapshot
	err     error
	deleted []machine.SnapshotID
}

func (s *snapshotter) ListSnapshots(context.Context) ([]machine.Snapshot, error) {
	return s.listed, s.err
}

func (s *snapshotter) DeleteSnapshot(_ context.Context, id machine.SnapshotID) error {
	s.deleted = append(s.deleted, id)
	return s.err
}

func TestListSnapshots_ReportsEverySnapshotWhole(t *testing.T) {
	m := &snapshotter{listed: []machine.Snapshot{{
		ID:      "snap",
		Machine: "src",
		Spec:    machine.SnapshotSpec{Image: pulled, RootDiskBytes: 1 << 20},
		Compat:  machine.SnapshotCompat{Driver: "fake", Version: "1", CPUModel: "cpu"},
	}}}

	resp, err := api.New(m, nil, nil, nil, nil, nil).ListSnapshots(context.Background(), &v1alpha2.ListSnapshotsRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Snapshots, 1)
	got := resp.Snapshots[0]
	assert.Equal(t, "snap", got.Id)
	assert.Equal(t, "src", got.MachineId)
	assert.Equal(t, pulled, got.ImageDigest)
	assert.Equal(t, "fake", got.Driver.Name)
	assert.Equal(t, "1", got.Driver.Version)
	assert.Equal(t, "cpu", got.CpuModel)
	assert.Equal(t, uint64(1<<20), got.RootDiskBytes)
}

func TestListSnapshots_MapsTheUseCaseOutcome(t *testing.T) {
	m := &snapshotter{err: machine.NewError(machine.ErrFailed, "list: boom")}

	resp, err := api.New(m, nil, nil, nil, nil, nil).ListSnapshots(context.Background(), &v1alpha2.ListSnapshotsRequest{})

	require.Error(t, err)
	assert.Nil(t, resp)
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, v1alpha2.KindOf(err))
}

func TestDeleteSnapshot_PassesTheIdentifierAndMapsTheUseCaseOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
	}{
		{"success", nil, v1alpha2.OutcomeKind_OUTCOME_KIND_SUCCESS},
		{"in use", machine.NewError(machine.ErrPreconditionFailed, "in use"), v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED},
		{"store failure", machine.NewError(machine.ErrFailed, "remove: boom"), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &snapshotter{err: tt.err}

			_, err := api.New(m, nil, nil, nil, nil, nil).DeleteSnapshot(context.Background(), &v1alpha2.DeleteSnapshotRequest{SnapshotId: "snap"})

			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
			assert.Equal(t, []machine.SnapshotID{"snap"}, m.deleted)
		})
	}
}

func TestDeleteSnapshot_RejectsAnEmptyIdentifierBeforeTheUseCase(t *testing.T) {
	m := &snapshotter{}
	server := api.New(m, nil, nil, nil, nil, nil)

	_, err := server.DeleteSnapshot(context.Background(), &v1alpha2.DeleteSnapshotRequest{})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.Empty(t, m.deleted, "the use case was not asked")

	_, err = server.DeleteSnapshot(context.Background(), &v1alpha2.DeleteSnapshotRequest{SnapshotId: "snap"}) // control
	assert.NoError(t, err)
	assert.Equal(t, []machine.SnapshotID{"snap"}, m.deleted)
}

func TestListSnapshots_ReportsTheKernelDigest(t *testing.T) {
	m := &snapshotter{listed: []machine.Snapshot{{ID: "snap", Compat: machine.SnapshotCompat{KernelDigest: "abc123"}}}}

	resp, err := api.New(m, nil, nil, nil, nil, nil).ListSnapshots(context.Background(), &v1alpha2.ListSnapshotsRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Snapshots, 1)
	assert.Equal(t, "abc123", resp.Snapshots[0].KernelDigest)
}
