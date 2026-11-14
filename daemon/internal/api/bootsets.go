package api

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

func wireBootSet(i machine.BootSetInfo) *v1alpha2.BootSet {
	origin := v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_CONFIGURED
	if i.Origin == machine.BootSetAdded {
		origin = v1alpha2.BootSetOrigin_BOOT_SET_ORIGIN_ADDED
	}
	return &v1alpha2.BootSet{Name: i.Name, KernelDigest: i.Set.Kernel.SHA256, InitrdDigest: i.Set.Initrd.SHA256, Origin: origin}
}

// bootSetChunks reads the two files of an AddBootSet stream, the messages
// after its first: the kernel's chunks, then the initrd's. A stream that ends
// cleanly is io.EOF for both; one that breaks is its error, so nothing is
// published.
type bootSetChunks struct {
	stream grpc.ClientStreamingServer[v1alpha2.AddBootSetRequest, v1alpha2.AddBootSetResponse]
	// pending is the first message of the initrd, read while the kernel's
	// reader looked for its end.
	pending []byte
	// inInitrd is set once a message of the initrd was read.
	inInitrd bool
	done     bool
	err      error
}

type bootSetFile struct {
	chunks *bootSetChunks
	initrd bool
	rest   []byte
}

func (f *bootSetFile) Read(p []byte) (int, error) {
	for len(f.rest) == 0 {
		chunk, err := f.chunks.next(f.initrd)
		if err != nil {
			return 0, err
		}
		f.rest = chunk
	}
	n := copy(p, f.rest)
	f.rest = f.rest[n:]
	return n, nil
}

// next returns the next chunk of the kernel or, for initrd, of the initrd. The
// kernel's reader ends at the first initrd chunk, which the initrd's reader
// then takes first.
func (c *bootSetChunks) next(initrd bool) ([]byte, error) {
	if initrd && c.pending != nil {
		chunk := c.pending
		c.pending = nil
		return chunk, nil
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.done {
		return nil, io.EOF
	}
	if !initrd && c.inInitrd {
		return nil, io.EOF
	}
	msg, err := c.stream.Recv()
	if err != nil {
		c.err = err
		if errors.Is(err, io.EOF) {
			c.done = true
		}
		return nil, err
	}
	switch part := msg.GetPart().(type) {
	case *v1alpha2.AddBootSetRequest_KernelChunk:
		if initrd {
			c.err = rejected("a kernel chunk after an initrd chunk")
			return nil, c.err
		}
		return part.KernelChunk, nil
	case *v1alpha2.AddBootSetRequest_InitrdChunk:
		c.inInitrd = true
		if !initrd {
			c.pending = part.InitrdChunk
			if c.pending == nil {
				c.pending = []byte{}
			}
			return nil, io.EOF
		}
		return part.InitrdChunk, nil
	default:
		c.err = rejected("a start message after the first")
		return nil, c.err
	}
}

func (s *Server) AddBootSet(stream grpc.ClientStreamingServer[v1alpha2.AddBootSetRequest, v1alpha2.AddBootSetResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return rejected("the stream is empty: the first message carries the name and the digests")
		}
		return err
	}
	start := first.GetStart()
	if start == nil {
		return rejected("the first message carries the name and the digests")
	}
	chunks := &bootSetChunks{stream: stream}
	info, err := s.machines.AddBootSet(stream.Context(), start.GetName(), machine.BootUpload{
		Kernel: machine.BootContent{SHA256: start.GetKernelDigest(), Content: &bootSetFile{chunks: chunks}},
		Initrd: machine.BootContent{SHA256: start.GetInitrdDigest(), Content: &bootSetFile{chunks: chunks, initrd: true}},
	})
	if err != nil {
		// A stream that broke or sent a message out of order ends the call
		// with its own error, not one of the store's.
		if chunks.err != nil && !errors.Is(chunks.err, io.EOF) {
			return chunks.err
		}
		return status(err)
	}
	return stream.SendAndClose(&v1alpha2.AddBootSetResponse{BootSet: wireBootSet(info)})
}

func (s *Server) RemoveBootSet(ctx context.Context, req *v1alpha2.RemoveBootSetRequest) (*v1alpha2.RemoveBootSetResponse, error) {
	if err := s.machines.RemoveBootSet(ctx, req.GetName()); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.RemoveBootSetResponse{}, nil
}
