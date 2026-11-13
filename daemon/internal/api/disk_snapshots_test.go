package api_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// snapKeeper is a machine.Service whose disk snapshot calls answer fixed
// values and record what they were asked.
type snapKeeper struct {
	machine.Service
	err       error
	snapshots []machine.DiskSnapshotInfo
	disks     []machine.DiskInfo

	snapshotted []string
	deleted     []machine.DiskSnapshotID
	plain       int
	from        []machine.DiskSnapshotID
	fromFS      []machine.Filesystem
}

func (k *snapKeeper) SnapshotDisk(_ context.Context, disk machine.DiskID, snapshot machine.DiskSnapshotID, request machine.RequestID) error {
	k.snapshotted = append(k.snapshotted, string(disk)+"/"+string(snapshot)+"/"+string(request))
	return k.err
}

func (k *snapKeeper) ListDiskSnapshots(context.Context) ([]machine.DiskSnapshotInfo, error) {
	return k.snapshots, k.err
}

func (k *snapKeeper) DeleteDiskSnapshot(_ context.Context, snapshot machine.DiskSnapshotID) error {
	k.deleted = append(k.deleted, snapshot)
	return k.err
}

func (k *snapKeeper) CreateDisk(context.Context, machine.DiskID, machine.RequestID, uint64, machine.Filesystem) error {
	k.plain++
	return k.err
}

func (k *snapKeeper) CreateDiskFromSnapshot(_ context.Context, _ machine.DiskID, _ machine.RequestID, _ uint64, fs machine.Filesystem, snapshot machine.DiskSnapshotID) error {
	k.from = append(k.from, snapshot)
	k.fromFS = append(k.fromFS, fs)
	return k.err
}

func (k *snapKeeper) ListDisks(context.Context) ([]machine.DiskInfo, error) { return k.disks, nil }

func serverOf(m machine.Service) *api.Server {
	return api.New(m, nil, nil, machine.DriverInfo{}, nil, nil)
}

func TestSnapshotDisk_PassesTheIdentifiersAndReportsTheSnapshot(t *testing.T) {
	at := time.Unix(1700000000, 0).UTC()
	m := &snapKeeper{snapshots: []machine.DiskSnapshotInfo{
		{DiskSnapshot: machine.DiskSnapshot{ID: "other", Disk: "x", SizeBytes: 1}},
		{DiskSnapshot: machine.DiskSnapshot{ID: "s1", Disk: "d", SizeBytes: 8 << 20, Filesystem: machine.FilesystemExt4, CreatedAt: at}, AllocatedBytes: 4096},
	}}

	resp, err := serverOf(m).SnapshotDisk(context.Background(), &v1alpha2.SnapshotDiskRequest{DiskId: "d", SnapshotId: "s1", RequestId: "r"})

	require.NoError(t, err)
	assert.Equal(t, []string{"d/s1/r"}, m.snapshotted)
	got := resp.GetDiskSnapshot()
	assert.Equal(t, "s1", got.GetId())
	assert.Equal(t, "d", got.GetDiskId())
	assert.Equal(t, uint64(8<<20), got.GetSizeBytes())
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_EXT4, got.GetFilesystem())
	assert.Equal(t, uint64(4096), got.GetAllocatedBytes())
	assert.True(t, at.Equal(got.GetCreatedAt().AsTime()))
}

func TestSnapshotDisk_MapsTheUseCaseOutcome(t *testing.T) {
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
	}{
		{"conflict", machine.NewError(machine.ErrConflict, "other request"), v1alpha2.OutcomeKind_OUTCOME_KIND_CONFLICT},
		{"not found", machine.NewError(machine.ErrNotFound, "no disk"), v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND},
		{"precondition", machine.NewError(machine.ErrPreconditionFailed, "faulted"), v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := serverOf(&snapKeeper{err: tt.err}).SnapshotDisk(context.Background(), &v1alpha2.SnapshotDiskRequest{DiskId: "d", SnapshotId: "s", RequestId: "r"})
			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
		})
	}
}

func TestListDiskSnapshots_ReportsEachSnapshot(t *testing.T) {
	m := &snapKeeper{snapshots: []machine.DiskSnapshotInfo{
		{DiskSnapshot: machine.DiskSnapshot{ID: "a", Disk: "d", SizeBytes: 1, Filesystem: machine.FilesystemNone}, AllocatedBytes: 7},
		{DiskSnapshot: machine.DiskSnapshot{ID: "b", Disk: "d", SizeBytes: 2, Filesystem: machine.FilesystemExt4}},
	}}

	resp, err := serverOf(m).ListDiskSnapshots(context.Background(), &v1alpha2.ListDiskSnapshotsRequest{})

	require.NoError(t, err)
	require.Len(t, resp.GetDiskSnapshots(), 2)
	assert.Equal(t, "a", resp.GetDiskSnapshots()[0].GetId())
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_NONE, resp.GetDiskSnapshots()[0].GetFilesystem())
	assert.Equal(t, uint64(7), resp.GetDiskSnapshots()[0].GetAllocatedBytes())
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_EXT4, resp.GetDiskSnapshots()[1].GetFilesystem())
}

func TestDeleteDiskSnapshot_PassesTheIdentifierAndRejectsAnEmptyOne(t *testing.T) {
	m := &snapKeeper{}
	_, err := serverOf(m).DeleteDiskSnapshot(context.Background(), &v1alpha2.DeleteDiskSnapshotRequest{SnapshotId: "s1"})
	require.NoError(t, err)
	assert.Equal(t, []machine.DiskSnapshotID{"s1"}, m.deleted)

	_, err = serverOf(m).DeleteDiskSnapshot(context.Background(), &v1alpha2.DeleteDiskSnapshotRequest{})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.Len(t, m.deleted, 1, "the use case was not asked")

	m.err = machine.NewError(machine.ErrPreconditionFailed, "in use")
	_, err = serverOf(m).DeleteDiskSnapshot(context.Background(), &v1alpha2.DeleteDiskSnapshotRequest{SnapshotId: "s1"})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, v1alpha2.KindOf(err))
}

func TestCreateDisk_FromASnapshotTakesTheSnapshotPathAndReportsItsFilesystem(t *testing.T) {
	m := &snapKeeper{disks: []machine.DiskInfo{{ID: "new", SizeBytes: 16 << 20, Filesystem: machine.FilesystemNone}}}

	resp, err := serverOf(m).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "new", RequestId: "r", SizeBytes: 16 << 20, FromSnapshotId: "s1"})

	require.NoError(t, err)
	assert.Equal(t, []machine.DiskSnapshotID{"s1"}, m.from)
	assert.Equal(t, []machine.Filesystem{""}, m.fromFS, "an unspecified filesystem is passed as such")
	assert.Zero(t, m.plain, "the plain create was not used")
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_NONE, resp.GetDisk().GetFilesystem(), "the disk reports the snapshot's filesystem")
	assert.Equal(t, uint64(16<<20), resp.GetDisk().GetSizeBytes())

	// The control: with no snapshot the plain create is used.
	m = &snapKeeper{}
	_, err = serverOf(m).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "new", RequestId: "r", SizeBytes: 1 << 20})
	require.NoError(t, err)
	assert.Equal(t, 1, m.plain)
	assert.Empty(t, m.from)
}

func TestCreateDisk_FromAnUnknownSnapshotIsNotFound(t *testing.T) {
	m := &snapKeeper{err: machine.NewError(machine.ErrNotFound, "no snapshot")}
	_, err := serverOf(m).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "new", RequestId: "r", SizeBytes: 1 << 20, FromSnapshotId: "gone"})
	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, v1alpha2.KindOf(err))
}

func TestSnapshotDiskRequest_ValidateRefusesAnEmptyOrBadIdentifier(t *testing.T) {
	valid := &v1alpha2.SnapshotDiskRequest{DiskId: "d", SnapshotId: "s", RequestId: "r"}
	require.NoError(t, valid.Validate(), "the setup: the control is valid")
	for name, req := range map[string]*v1alpha2.SnapshotDiskRequest{
		"disk":     {SnapshotId: "s", RequestId: "r"},
		"snapshot": {DiskId: "d", RequestId: "r"},
		"request":  {DiskId: "d", SnapshotId: "s"},
	} {
		t.Run(name, func(t *testing.T) { assert.Error(t, req.Validate()) })
	}
}
