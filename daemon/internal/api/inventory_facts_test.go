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

// A Machine reports the image it runs in every state it can be listed in.
func TestListMachines_ReportsTheImageOfAMachineInAnyState(t *testing.T) {
	m := &lister{all: []machine.Machine{
		{ID: "running", State: machine.StateRunning, Spec: machine.Spec{Image: "sha256:aaa"}},
		{ID: "stopped", State: machine.StateStopped, Spec: machine.Spec{Image: "sha256:bbb"}},
		{ID: "failed", State: machine.StateFaulted, Spec: machine.Spec{Image: "sha256:ccc"}},
		{ID: "noimage"},
	}}

	resp, err := api.New(m, nil, nil, nil, nil, nil).ListMachines(context.Background(), &v1alpha2.ListMachinesRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Machines, 4)
	assert.Equal(t, "sha256:aaa", resp.Machines[0].ImageDigest)
	assert.Equal(t, "sha256:bbb", resp.Machines[1].ImageDigest)
	assert.Equal(t, "sha256:ccc", resp.Machines[2].ImageDigest)
	assert.Empty(t, resp.Machines[3].ImageDigest, "control: a record with none reports none")
}

func TestListImages_ReportsTheSizeAndTheTimeItWasStored(t *testing.T) {
	stored := time.Date(2026, 5, 1, 12, 30, 0, 500, time.UTC)
	m := &imager{listed: []machine.ImageInfo{
		{Digest: "sha256:known", SizeBytes: 8192, CreatedAt: stored},
		{Digest: "sha256:earlier", SizeBytes: 4096},
	}}

	resp, err := api.New(m, nil, nil, nil, nil, nil).ListImages(context.Background(), &v1alpha2.ListImagesRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Images, 2)
	assert.Equal(t, uint64(8192), resp.Images[0].SizeBytes)
	require.NotNil(t, resp.Images[0].CreatedAt)
	assert.True(t, resp.Images[0].CreatedAt.AsTime().Equal(stored))
	assert.Equal(t, uint64(4096), resp.Images[1].SizeBytes)
	assert.Nil(t, resp.Images[1].CreatedAt, "an image with no time reports it unset and not as the epoch")
}

func TestListSnapshots_ReportsTheSizeAndTheTimeItWasTaken(t *testing.T) {
	taken := time.Date(2026, 5, 2, 8, 0, 0, 0, time.UTC)
	m := &snapshotter{
		listed: []machine.Snapshot{{ID: "known", CreatedAt: taken}, {ID: "earlier"}},
		sizes:  map[machine.SnapshotID]uint64{"known": 1 << 30, "earlier": 512},
	}

	resp, err := api.New(m, nil, nil, nil, nil, nil).ListSnapshots(context.Background(), &v1alpha2.ListSnapshotsRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Snapshots, 2)
	assert.Equal(t, uint64(1<<30), resp.Snapshots[0].SizeBytes)
	require.NotNil(t, resp.Snapshots[0].CreatedAt)
	assert.True(t, resp.Snapshots[0].CreatedAt.AsTime().Equal(taken))
	assert.Equal(t, uint64(512), resp.Snapshots[1].SizeBytes)
	assert.Nil(t, resp.Snapshots[1].CreatedAt, "a snapshot with no time reports it unset")
}
