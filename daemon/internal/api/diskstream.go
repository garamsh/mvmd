package api

import (
	"context"
	"errors"
	"io"
	"time"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

// defaultDiskStreamIdle is how long a disk stream may wait on its peer for one
// message before the call ends (ADR 0072).
const defaultDiskStreamIdle = 2 * time.Minute

// WithDiskStreamIdle sets how long ReadDisk and CreateDiskFromStream wait on
// the peer for one message, a send that does not return or a receive that does
// not arrive, before the call is ended and the disk it holds is freed. Without
// it the wait is two minutes.
func WithDiskStreamIdle(d time.Duration) Option {
	return func(s *Server) { s.diskStreamIdle = d }
}

// idle bounds each wait of a stream on its peer. A wait that outlasts d cancels
// the call's context, which the lifecycle watches, and returns an error for the
// stream to end with; the goroutine of the wait ends when the handler returns
// and gRPC closes the stream.
type idle struct {
	d      time.Duration
	cancel context.CancelFunc
}

var errIdle = v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, "the peer sent or took nothing for too long: the stream is ended")

func (g idle) wait(f func() error) error {
	done := make(chan error, 1)
	go func() { done <- f() }()
	timer := time.NewTimer(g.d)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		g.cancel()
		return errIdle
	}
}

// diskSink sends a ReadDisk stream's parts as messages.
type diskSink struct {
	stream grpc.ServerStreamingServer[v1alpha2.ReadDiskResponse]
	idle   idle
}

func (k diskSink) send(part *v1alpha2.ReadDiskResponse) error {
	return k.idle.wait(func() error { return k.stream.Send(part) })
}

func (k diskSink) Header(sizeBytes uint64, fs machine.Filesystem) error {
	return k.send(&v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Header{Header: &v1alpha2.DiskStreamHeader{SizeBytes: sizeBytes, Filesystem: filesystemWire(fs)}}})
}

func (k diskSink) Extent(offset uint64, data []byte) error {
	return k.send(&v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Extent{Extent: &v1alpha2.DiskExtent{Offset: offset, Data: data}}})
}

func (k diskSink) Digest(sha256 string) error {
	return k.send(&v1alpha2.ReadDiskResponse{Part: &v1alpha2.ReadDiskResponse_Digest{Digest: &v1alpha2.DiskStreamDigest{Sha256: sha256}}})
}

func (s *Server) ReadDisk(req *v1alpha2.ReadDiskRequest, stream grpc.ServerStreamingServer[v1alpha2.ReadDiskResponse]) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	source := machine.DiskReadSource{Disk: machine.DiskID(req.GetDiskId()), Snapshot: machine.DiskSnapshotID(req.GetSnapshotId())}
	if err := s.machines.ReadDisk(ctx, source, diskSink{stream: stream, idle: idle{s.idleFor(), cancel}}); err != nil {
		return status(err)
	}
	return nil
}

// idleFor is the server's wait on a stream's peer.
func (s *Server) idleFor() time.Duration {
	if s.diskStreamIdle > 0 {
		return s.diskStreamIdle
	}
	return defaultDiskStreamIdle
}

// streamExtents yields the extent messages of a CreateDiskFromStream stream
// after its first, and the digest that ends them. A stream that ends with its
// digest and then closes is io.EOF; one that breaks, ends without a digest, or
// sends a message out of order is err, which ends the call.
type streamExtents struct {
	stream grpc.ClientStreamingServer[v1alpha2.CreateDiskFromStreamRequest, v1alpha2.CreateDiskFromStreamResponse]
	idle   idle
	digest string
	err    error
}

func (e *streamExtents) recv() (*v1alpha2.CreateDiskFromStreamRequest, error) {
	var msg *v1alpha2.CreateDiskFromStreamRequest
	err := e.idle.wait(func() (err error) {
		msg, err = e.stream.Recv()
		return err
	})
	return msg, err
}

func (e *streamExtents) Next() (uint64, []byte, error) {
	msg, err := e.recv()
	if errors.Is(err, io.EOF) {
		err = rejected("the stream ended without the digest that ends it")
	}
	if err != nil {
		e.err = err
		return 0, nil, err
	}
	switch part := msg.GetPart().(type) {
	case *v1alpha2.CreateDiskFromStreamRequest_Extent:
		return part.Extent.GetOffset(), part.Extent.GetData(), nil
	case *v1alpha2.CreateDiskFromStreamRequest_Digest:
		e.digest = part.Digest.GetSha256()
		// The digest is the last message: the stream is closed after it.
		if _, err := e.recv(); !errors.Is(err, io.EOF) {
			if err == nil {
				err = rejected("a message after the digest")
			}
			e.err = err
			return 0, nil, err
		}
		return 0, nil, io.EOF
	}
	e.err = rejected("a start message after the first")
	return 0, nil, e.err
}

func (e *streamExtents) Digest() string { return e.digest }

func (s *Server) CreateDiskFromStream(stream grpc.ClientStreamingServer[v1alpha2.CreateDiskFromStreamRequest, v1alpha2.CreateDiskFromStreamResponse]) error {
	ctx, cancel := context.WithCancel(stream.Context())
	defer cancel()
	extents := &streamExtents{stream: stream, idle: idle{s.idleFor(), cancel}}
	first, err := extents.recv()
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
	err = s.machines.CreateDiskFromStream(ctx, disk, machine.RequestID(start.GetRequestId()), start.GetSizeBytes(), fs, extents)
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
