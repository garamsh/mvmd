package api_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/mvmd/daemon/internal/api"
	"github.com/garamsh/mvmd/daemon/internal/catalog"
	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/daemon/internal/machine/memory"
	"github.com/garamsh/mvmd/daemon/internal/nodecapacity"
	capacitymemory "github.com/garamsh/mvmd/daemon/internal/nodecapacity/memory"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// streamNode is a runtime API server over the real Machine use case on the
// in-memory disks, and the disks, whose bytes a test sets and reads.
func streamNode(t *testing.T) (v1alpha2.RuntimeServiceClient, *memory.Disk) {
	t.Helper()
	info := machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu"}
	images, snapshots, store, disks := memory.NewImageStore(), memory.NewSnapshotStore(), memory.NewStateStore(), memory.NewDisk()
	nodeCatalog := catalog.NewService(images, snapshots, store)
	svc := machine.NewService(memory.NewDriver(info), memory.NewSupervision(), memory.NewNetwork(), disks, images, snapshots,
		memory.NewGuest(0), memory.NewWallClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)), store, memory.NewStartPermitter(),
		machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), memory.NewEvents(), machine.WithKernels(memory.Kernels()), machine.WithCatalog(nodeCatalog))
	srv := api.New(svc, nil, nil, info, capacitymemory.NewReader(nodecapacity.Capacity{}), nil, api.WithCatalog(nodeCatalog))
	return dialThroughInterceptors(t, srv), disks
}

// readDisk collects a ReadDisk stream: its parts in order, and the call's
// outcome.
func readDisk(t *testing.T, client v1alpha2.RuntimeServiceClient, req *v1alpha2.ReadDiskRequest) ([]*v1alpha2.ReadDiskResponse, error) {
	t.Helper()
	stream, err := client.ReadDisk(context.Background(), req)
	require.NoError(t, err)
	var parts []*v1alpha2.ReadDiskResponse
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			return parts, nil
		}
		if err != nil {
			return parts, err
		}
		parts = append(parts, msg)
	}
}

func streamStart(disk string, size uint64, data []byte) *v1alpha2.CreateDiskFromStreamRequest {
	return &v1alpha2.CreateDiskFromStreamRequest{Part: &v1alpha2.CreateDiskFromStreamRequest_Start{Start: &v1alpha2.CreateDiskFromStreamStart{
		DiskId: disk, RequestId: "r-" + disk, SizeBytes: size, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE, Sha256: hexSum(data),
	}}}
}

func streamExtent(offset uint64, data []byte) *v1alpha2.CreateDiskFromStreamRequest {
	return &v1alpha2.CreateDiskFromStreamRequest{Part: &v1alpha2.CreateDiskFromStreamRequest_Extent{Extent: &v1alpha2.DiskExtent{Offset: offset, Data: data}}}
}

func createFromStream(t *testing.T, client v1alpha2.RuntimeServiceClient, msgs ...*v1alpha2.CreateDiskFromStreamRequest) (*v1alpha2.CreateDiskFromStreamResponse, error) {
	t.Helper()
	stream, err := client.CreateDiskFromStream(context.Background())
	require.NoError(t, err)
	for _, m := range msgs {
		if err := stream.Send(m); err != nil {
			break // the call has ended: its outcome is what CloseAndRecv returns
		}
	}
	return stream.CloseAndRecv()
}

// A disk read from one node and made on another is the same bytes: the header,
// the extents and the digest of the one are what the other takes.
func TestReadDisk_ThenCreateDiskFromStream_ReachTheService(t *testing.T) {
	ctx := context.Background()
	from, fromDisks := streamNode(t)
	_, err := from.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "d", RequestId: "r", SizeBytes: 4096, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	data := make([]byte, 4096)
	copy(data[100:], "payload")
	fromDisks.Fill("/disks/d", data)

	parts, err := readDisk(t, from, &v1alpha2.ReadDiskRequest{DiskId: "d"})
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(parts), 3)
	assert.Equal(t, uint64(4096), parts[0].GetHeader().GetSizeBytes(), "the header comes first")
	assert.Equal(t, v1alpha2.Filesystem_FILESYSTEM_NONE, parts[0].GetHeader().GetFilesystem())
	last := parts[len(parts)-1].GetDigest()
	require.NotNil(t, last, "the digest ends the stream")
	assert.Equal(t, hexSum(data), last.GetSha256())

	to, toDisks := streamNode(t)
	msgs := []*v1alpha2.CreateDiskFromStreamRequest{{Part: &v1alpha2.CreateDiskFromStreamRequest_Start{Start: &v1alpha2.CreateDiskFromStreamStart{
		DiskId: "copy", RequestId: "r-c", SizeBytes: parts[0].GetHeader().GetSizeBytes(), Filesystem: parts[0].GetHeader().GetFilesystem(), Sha256: last.GetSha256()}}}}
	for _, p := range parts[1 : len(parts)-1] {
		msgs = append(msgs, streamExtent(p.GetExtent().GetOffset(), p.GetExtent().GetData()))
	}
	resp, err := createFromStream(t, to, msgs...)
	require.NoError(t, err)
	assert.Equal(t, "copy", resp.GetDisk().GetId())
	assert.Equal(t, uint64(4096), resp.GetDisk().GetSizeBytes())
	assert.Equal(t, data, toDisks.Bytes("/disks/copy"))

	again, err := createFromStream(t, to, msgs[0])
	require.NoError(t, err, "a repeat succeeds whatever its stream carries")
	assert.Equal(t, "copy", again.GetDisk().GetId())
}

func TestReadDisk_RefusalsReachTheCallerWithTheirKind(t *testing.T) {
	client, _ := streamNode(t)
	_, err := client.CreateDisk(context.Background(), &v1alpha2.CreateDiskRequest{DiskId: "d", RequestId: "r", SizeBytes: 4096, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	for name, tc := range map[string]struct {
		req  *v1alpha2.ReadDiskRequest
		kind v1alpha2.OutcomeKind
	}{
		"neither source":      {&v1alpha2.ReadDiskRequest{}, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED},
		"both sources":        {&v1alpha2.ReadDiskRequest{DiskId: "d", SnapshotId: "s"}, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED},
		"an unknown disk":     {&v1alpha2.ReadDiskRequest{DiskId: "absent"}, v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND},
		"an unknown snapshot": {&v1alpha2.ReadDiskRequest{SnapshotId: "absent"}, v1alpha2.OutcomeKind_OUTCOME_KIND_UNSUPPORTED},
	} {
		t.Run(name, func(t *testing.T) {
			parts, err := readDisk(t, client, tc.req)
			assert.Equal(t, tc.kind, v1alpha2.KindOf(err), "%v", err)
			assert.Empty(t, parts)
		})
	}

	parts, err := readDisk(t, client, &v1alpha2.ReadDiskRequest{DiskId: "d"})
	require.NoError(t, err, "control: an existing disk is read")
	assert.NotEmpty(t, parts)
}

func TestCreateDiskFromStream_RefusalsReachTheCallerAndLeaveNothing(t *testing.T) {
	data := []byte("0123456789abcdef")
	for name, msgs := range map[string][]*v1alpha2.CreateDiskFromStreamRequest{
		"bytes that are not the digest": {streamStart("n", 16, data), streamExtent(0, []byte("0123456789abcdeX"))},
		"a stream cut short":            {streamStart("n", 16, data), streamExtent(0, data[:8])},
		"an extent past the size":       {streamStart("n", 16, data), streamExtent(8, data)},
		"no start":                      {streamExtent(0, data)},
		"a second start":                {streamStart("n", 16, data), streamStart("n", 16, data)},
		"no messages":                   nil,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := streamNode(t)

			_, err := createFromStream(t, client, msgs...)

			assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "%v", err)
			all, lerr := client.ListDisks(context.Background(), &v1alpha2.ListDisksRequest{})
			require.NoError(t, lerr)
			assert.Empty(t, all.GetDisks(), "nothing was published")

			_, err = createFromStream(t, client, streamStart("n", 16, data), streamExtent(0, data))
			require.NoError(t, err, "control: the whole stream in order is accepted")
		})
	}
}
