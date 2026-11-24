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
func streamNode(t *testing.T, opts ...api.Option) (v1alpha2.RuntimeServiceClient, *memory.Disk) {
	t.Helper()
	info := machine.DriverInfo{Name: "inmem", Version: "1", CPUModel: "cpu"}
	images, snapshots, store, disks := memory.NewImageStore(), memory.NewSnapshotStore(), memory.NewStateStore(), memory.NewDisk()
	nodeCatalog := catalog.NewService(images, snapshots, store)
	svc := machine.NewService(memory.NewDriver(info), memory.NewSupervision(), memory.NewNetwork(), disks, images, snapshots,
		memory.NewGuest(0), memory.NewWallClock(time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)), store, memory.NewStartPermitter(),
		machine.NewUIDs(machine.UIDRange{First: 1000, Count: 256}), memory.NewEvents(), machine.WithKernels(memory.Kernels()), machine.WithCatalog(nodeCatalog))
	srv := api.New(svc, nil, nil, info, capacitymemory.NewReader(nodecapacity.Capacity{}), nil, append([]api.Option{api.WithCatalog(nodeCatalog)}, opts...)...)
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

func streamStart(disk string, size uint64) *v1alpha2.CreateDiskFromStreamRequest {
	return &v1alpha2.CreateDiskFromStreamRequest{Part: &v1alpha2.CreateDiskFromStreamRequest_Start{Start: &v1alpha2.CreateDiskFromStreamStart{
		DiskId: disk, RequestId: "r-" + disk, SizeBytes: size, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE,
	}}}
}

func streamDigest(sum string) *v1alpha2.CreateDiskFromStreamRequest {
	return &v1alpha2.CreateDiskFromStreamRequest{Part: &v1alpha2.CreateDiskFromStreamRequest_Digest{Digest: &v1alpha2.DiskStreamDigest{Sha256: sum}}}
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
		DiskId: "copy", RequestId: "r-c", SizeBytes: parts[0].GetHeader().GetSizeBytes(), Filesystem: parts[0].GetHeader().GetFilesystem()}}}}
	for _, p := range parts[1 : len(parts)-1] {
		msgs = append(msgs, streamExtent(p.GetExtent().GetOffset(), p.GetExtent().GetData()))
	}
	msgs = append(msgs, streamDigest(last.GetSha256()))
	resp, err := createFromStream(t, to, msgs...)
	require.NoError(t, err)
	assert.Equal(t, "copy", resp.GetDisk().GetId())
	assert.Equal(t, uint64(4096), resp.GetDisk().GetSizeBytes())
	assert.Equal(t, data, toDisks.Bytes("/disks/copy"))

	again, err := createFromStream(t, to, msgs...)
	require.NoError(t, err, "a retry streams again and succeeds")
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
	sum := hexSum(data)
	start, whole, tail := streamStart("n", 16), streamExtent(0, data), streamDigest(sum)
	for name, msgs := range map[string][]*v1alpha2.CreateDiskFromStreamRequest{
		"bytes that are not the digest":   {start, streamExtent(0, []byte("0123456789abcdeX")), tail},
		"a stream cut short":              {start, streamExtent(0, data[:8]), tail},
		"an extent past the size":         {start, streamExtent(8, data), tail},
		"no start":                        {whole, tail},
		"a second start":                  {start, whole, start, tail},
		"a second start after the digest": {start, whole, tail, start},
		"an extent after the digest":      {start, whole, tail, whole},
		"a second digest":                 {start, whole, tail, tail},
		"no digest":                       {start, whole},
		"a digest that is not a digest":   {start, whole, streamDigest("abc")},
		"a digest only":                   {tail},
		"no messages":                     nil,
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := streamNode(t)

			_, err := createFromStream(t, client, msgs...)

			assert.Equal(t, v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, v1alpha2.KindOf(err), "%v", err)
			all, lerr := client.ListDisks(context.Background(), &v1alpha2.ListDisksRequest{})
			require.NoError(t, lerr)
			assert.Empty(t, all.GetDisks(), "nothing was published")

			_, err = createFromStream(t, client, start, whole, tail)
			require.NoError(t, err, "control: the whole stream in order is accepted")
		})
	}
}

// A sender that stops sending does not hold the disk: after the idle bound the
// call ends with nothing published, and the disk can be made.
func TestCreateDiskFromStream_AStalledSenderEndsTheCallAndFreesTheDisk(t *testing.T) {
	client, _ := streamNode(t, api.WithDiskStreamIdle(200*time.Millisecond))
	data := []byte("0123456789abcdef")
	stream, err := client.CreateDiskFromStream(context.Background())
	require.NoError(t, err)
	require.NoError(t, stream.Send(streamStart("n", 16)))
	require.NoError(t, stream.Send(streamExtent(0, data)))

	done := make(chan error, 1)
	go func() { done <- stream.RecvMsg(&v1alpha2.CreateDiskFromStreamResponse{}) }() // no CloseSend: the sender stalls
	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("the call did not end though the sender sent nothing more")
	}

	all, err := client.ListDisks(context.Background(), &v1alpha2.ListDisksRequest{})
	require.NoError(t, err)
	assert.Empty(t, all.GetDisks())
	_, err = createFromStream(t, client, streamStart("n", 16), streamExtent(0, data), streamDigest(hexSum(data)))
	require.NoError(t, err, "control: the disk is made once the stalled call has ended")
}

// A reader that stops reading does not hold the disk: after the idle bound the
// read ends, and the disk can be deleted.
func TestReadDisk_AStalledReaderEndsTheCallAndFreesTheDisk(t *testing.T) {
	client, disks := streamNode(t, api.WithDiskStreamIdle(200*time.Millisecond))
	ctx := context.Background()
	const size = 8 << 20
	_, err := client.CreateDisk(ctx, &v1alpha2.CreateDiskRequest{DiskId: "d", RequestId: "r", SizeBytes: size, Filesystem: v1alpha2.Filesystem_FILESYSTEM_NONE})
	require.NoError(t, err)
	big := make([]byte, size)
	for i := range big {
		big[i] = byte(i%251 + 1)
	}
	disks.Fill("/disks/d", big)

	stream, err := client.ReadDisk(ctx, &v1alpha2.ReadDiskRequest{DiskId: "d"})
	require.NoError(t, err)
	_, err = stream.Recv() // the call is running; the reader now reads nothing more

	require.NoError(t, err)
	require.Eventually(t, func() bool {
		_, err := client.DeleteDisk(ctx, &v1alpha2.DeleteDiskRequest{DiskId: "d"})
		return err == nil
	}, 10*time.Second, 50*time.Millisecond, "the stalled read let go of the disk")
}
