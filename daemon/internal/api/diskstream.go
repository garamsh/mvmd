package api

import (
	"errors"
	"io"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// diskSink sends a ReadDisk stream's parts as messages.
type diskSink struct {
	stream grpc.ServerStreamingServer[v1alpha2.ReadDiskResponse]
}

func (k diskSink) Header(sizeBytes uint64, fs machine.Filesystem) error {
	return k.stream.Send(&v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Header{Header: &v1alpha2.DiskStreamHeader{SizeBytes: sizeBytes, Filesystem: filesystemWire(fs)}}})
}

func (k diskSink) Extent(offset uint64, data []byte) error {
	return k.stream.Send(&v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Extent{Extent: &v1alpha2.DiskExtent{Offset: offset, Data: data}}})
}

func (k diskSink) Digest(sha256 string) error {
	return k.stream.Send(&v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Digest{Digest: &v1alpha2.DiskStreamDigest{Sha256: sha256}}})
}

func (s *Server) ReadDisk(req *v1alpha2.ReadDiskRequest, stream grpc.ServerStreamingServer[v1alpha2.ReadDiskResponse]) error {
	source := machine.DiskReadSource{Disk: machine.DiskID(req.GetDiskId()), Snapshot: machine.DiskSnapshotID(req.GetSnapshotId())}
	if err := s.machines.ReadDisk(stream.Context(), source, diskSink{stream}); err != nil {
		return status(err)
	}
	return nil
}

// streamExtents yields the extent messages of a CreateDiskFromStream stream
// after its first. A stream that ends cleanly is io.EOF; one that breaks, or
// sends a message out of order, is err, which ends the call.
type streamExtents struct {
	stream grpc.ClientStreamingServer[v1alpha2.CreateDiskFromStreamRequest, v1alpha2.CreateDiskFromStreamResponse]
	err    error
}

func (e *streamExtents) Next() (uint64, []byte, error) {
	msg, err := e.stream.Recv()
	if err != nil {
		e.err = err
		return 0, nil, err
	}
	extent := msg.GetExtent()
	if extent == nil {
		e.err = rejected("a start message after the first")
		return 0, nil, e.err
	}
	return extent.GetOffset(), extent.GetData(), nil
}

func (s *Server) CreateDiskFromStream(stream grpc.ClientStreamingServer[v1alpha2.CreateDiskFromStreamRequest, v1alpha2.CreateDiskFromStreamResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return rejected("the stream is empty: the first message carries the disk's description")
		}
		return err
	}
	start := first.GetStart()
	if start == nil {
		return rejected("the first message carries the disk's description")
	}
	fs, err := filesystemOf(start.GetFilesystem())
	if err != nil {
		return err
	}
	disk := machine.DiskID(start.GetDiskId())
	extents := &streamExtents{stream: stream}
	err = s.machines.CreateDiskFromStream(stream.Context(), disk, machine.RequestID(start.GetRequestId()), start.GetSizeBytes(), fs, start.GetSha256(), extents)
	if err != nil {
		// A stream that broke or sent a message out of order ends the call
		// with its own error, not one of the disk's.
		if extents.err != nil && !errors.Is(extents.err, io.EOF) {
			return extents.err
		}
		return status(err)
	}
	return stream.SendAndClose(&v1alpha2.CreateDiskFromStreamResponse{Disk: &v1alpha2.Disk{Id: string(disk), SizeBytes: start.GetSizeBytes(), Filesystem: filesystemWire(fs)}})
}
