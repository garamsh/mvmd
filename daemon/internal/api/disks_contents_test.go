package api_test

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func TestCreateDisk_ContentsAreRefusedNamingTheFieldOrPassedOn(t *testing.T) {
	const oneMiB = 1 << 20
	tests := []struct {
		name     string
		req      *v1alpha2.CreateDiskRequest
		refused  bool
		wantSize uint64
	}{
		{"over 1 MiB", &v1alpha2.CreateDiskRequest{Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE, Contents: make([]byte, oneMiB+1)}, true, 0},
		{"exactly 1 MiB (control)", &v1alpha2.CreateDiskRequest{Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE, Contents: make([]byte, oneMiB)}, false, oneMiB},
		{"with ext4", &v1alpha2.CreateDiskRequest{Filesystem: v1alpha2.Filesystem_FILESYSTEM_EXT4, Contents: []byte("seed")}, true, 0},
		{"with the filesystem unspecified", &v1alpha2.CreateDiskRequest{Contents: []byte("seed")}, true, 0},
		{"with a snapshot", &v1alpha2.CreateDiskRequest{Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE, FromSnapshotId: "s", Contents: []byte("seed")}, true, 0},
		{"with none (control)", &v1alpha2.CreateDiskRequest{Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE, Contents: []byte("seed")}, false, 512},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := &diskKeeper{}
			tt.req.DiskId, tt.req.RequestId = "seed", "req"

			resp, err := api.New(m, nil, nil, machine.DriverInfo{}, nil, nil).CreateDisk(context.Background(), tt.req)

			if tt.refused {
				require.Error(t, err)
				assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err))
				assert.True(t, strings.Contains(err.Error(), "contents"), "the reason names the field: %v", err)
				assert.Empty(t, m.created, "the use case was not asked")
				return
			}
			require.NoError(t, err)
			require.Len(t, m.created, 1)
			assert.Equal(t, machine.ContentsDigest(tt.req.Contents), m.created[0].ContentsDigest)
			assert.Equal(t, tt.wantSize, resp.GetDisk().GetSizeBytes())
			assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_NONE, resp.GetDisk().GetFilesystem())
		})
	}
}
