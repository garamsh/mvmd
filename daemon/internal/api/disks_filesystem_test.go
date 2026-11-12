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

func TestCreateDisk_PassesTheFilesystemAndReportsIt(t *testing.T) {
	tests := []struct {
		name  string
		wire  v1alpha2.Filesystem
		asked machine.Filesystem
		shown v1alpha2.Filesystem
	}{
		{"none", v1alpha2.Filesystem_FILESYSTEM_NONE, machine.FilesystemNone, v1alpha2.Filesystem_FILESYSTEM_NONE},
		{"ext4", v1alpha2.Filesystem_FILESYSTEM_EXT4, machine.FilesystemExt4, v1alpha2.Filesystem_FILESYSTEM_EXT4},
		{"unspecified is ext4", v1alpha2.Filesystem_FILESYSTEM_UNSPECIFIED, "", v1alpha2.Filesystem_FILESYSTEM_EXT4},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &diskKeeper{}

			resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "req", SizeBytes: 1 << 20, Filesystem: tt.wire})

			require.NoError(t, err)
			assert.Equal(t, []machine.DiskEntry{{ID: "data", Request: "req", SizeBytes: 1 << 20, Filesystem: tt.asked}}, m.created)
			assert.Equal(t, tt.shown, resp.GetDisk().GetFilesystem())
		})
	}
}

func TestCreateDisk_RejectsAFilesystemItDoesNotKnow(t *testing.T) {
	m := &diskKeeper{}

	_, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "data", RequestId: "req", SizeBytes: 1, Filesystem: 99})

	assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
	assert.Empty(t, m.created, "the use case was not asked")
}

func TestListDisks_ReportsTheFilesystemOfEveryDisk(t *testing.T) {
	m := &diskKeeper{listed: []machine.DiskInfo{{ID: "a", Filesystem: machine.FilesystemNone}, {ID: "b", Filesystem: machine.FilesystemExt4}}}

	resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).ListDisks(context.Background(), &v1alpha2.ListDisksRequest{})

	require.NoError(t, err)
	require.Len(t, resp.Disks, 2)
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_NONE, resp.Disks[0].Filesystem)
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_EXT4, resp.Disks[1].Filesystem)
}
