package api

import (
	"context"
	"errors"
	"io"

	"google.golang.org/grpc"

	"github.com/garamsh/mvmd/daemon/internal/machine"
	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

var execStatuses = map[machine.ExecStatus]v1alpha2.ExecStatus{
	machine.ExecNotStarted: v1alpha2.ExecStatus_EXEC_STATUS_NOT_STARTED,
	machine.ExecStarted:    v1alpha2.ExecStatus_EXEC_STATUS_STARTED,
	machine.ExecExited:     v1alpha2.ExecStatus_EXEC_STATUS_EXITED,
	machine.ExecCutShort:   v1alpha2.ExecStatus_EXEC_STATUS_CUT_SHORT,
}

var outputStreams = map[machine.OutputStream]v1alpha2.OutputStream{
	machine.OutputStdout: v1alpha2.OutputStream_OUTPUT_STREAM_STDOUT,
	machine.OutputStderr: v1alpha2.OutputStream_OUTPUT_STREAM_STDERR,
}

func execRecordToWire(r machine.ExecRecord) *v1alpha2.ExecRecord {
	return &v1alpha2.ExecRecord{ExecId: string(r.ID), Status: execStatuses[r.Status], Reason: r.Reason, ExitCode: r.ExitCode}
}

func (s *Server) Exec(ctx context.Context, req *v1alpha2.ExecRequest) (*v1alpha2.ExecResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	spec := machine.ExecSpec{Command: req.GetCommand(), Env: req.GetEnv(), WorkingDir: req.GetWorkingDir()}
	record, err := s.machines.Exec(ctx, id, machine.ExecID(req.GetExecId()), machine.RequestID(req.GetRequestId()), spec)
	if err != nil {
		return nil, status(err)
	}
	return &v1alpha2.ExecResponse{Record: execRecordToWire(record)}, nil
}

func (s *Server) ReadExecOutput(req *v1alpha2.ReadExecOutputRequest, stream grpc.ServerStreamingServer[v1alpha2.ReadExecOutputResponse]) error {
	id, err := target(req.GetMachineId())
	if err != nil {
		return err
	}
	if req.GetExecId() == "" {
		return rejected("exec_id is empty")
	}
	send := func(c machine.OutputChunk) error {
		return stream.Send(&v1alpha2.ReadExecOutputResponse{Part: &v1alpha2.ReadExecOutputResponse_Chunk{
			Chunk: &v1alpha2.OutputChunk{Stream: outputStreams[c.Stream], Offset: c.Offset, Data: c.Data},
		}})
	}
	record, err := s.machines.ReadExecOutput(stream.Context(), id, machine.ExecID(req.GetExecId()), req.GetAfterOffset(), send)
	if err != nil {
		return status(err)
	}
	if record.Status == machine.ExecStarted {
		return nil // the command runs: the client reads again from the last offset it holds
	}
	return stream.Send(&v1alpha2.ReadExecOutputResponse{Part: &v1alpha2.ReadExecOutputResponse_Record{Record: execRecordToWire(record)}})
}

func (s *Server) SignalExec(ctx context.Context, req *v1alpha2.SignalExecRequest) (*v1alpha2.SignalExecResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.SignalExec(ctx, id, machine.ExecID(req.GetExecId()), req.GetSignal()); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.SignalExecResponse{}, nil
}

func (s *Server) ReadFile(req *v1alpha2.ReadFileRequest, stream grpc.ServerStreamingServer[v1alpha2.ReadFileResponse]) error {
	id, err := target(req.GetMachineId())
	if err != nil {
		return err
	}
	send := func(chunk []byte) error { return stream.Send(&v1alpha2.ReadFileResponse{Chunk: chunk}) }
	return status(s.machines.ReadFile(stream.Context(), id, req.GetPath(), send))
}

// chunkReader reads the content of a WriteFile stream: the chunks after its
// first message. A stream that ends cleanly is io.EOF; one that breaks is its
// error, so the lifecycle leaves the old content.
type chunkReader struct {
	stream grpc.ClientStreamingServer[v1alpha2.WriteFileRequest, v1alpha2.WriteFileResponse]
	rest   []byte
}

func (r *chunkReader) Read(p []byte) (int, error) {
	for len(r.rest) == 0 {
		msg, err := r.stream.Recv()
		if err != nil {
			return 0, err
		}
		if _, isStart := msg.GetPart().(*v1alpha2.WriteFileRequest_Start); isStart {
			return 0, errors.New("a start message after the first")
		}
		r.rest = msg.GetChunk()
	}
	n := copy(p, r.rest)
	r.rest = r.rest[n:]
	return n, nil
}

func (s *Server) WriteFile(stream grpc.ClientStreamingServer[v1alpha2.WriteFileRequest, v1alpha2.WriteFileResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return rejected("the stream is empty: the first message carries the path")
		}
		return err
	}
	start := first.GetStart()
	if start == nil {
		return rejected("the first message carries the Machine and the path")
	}
	id, err := target(start.GetMachineId())
	if err != nil {
		return err
	}
	if err := s.machines.WriteFile(stream.Context(), id, start.GetPath(), &chunkReader{stream: stream}); err != nil {
		return status(err)
	}
	return stream.SendAndClose(&v1alpha2.WriteFileResponse{})
}

// consoleReadSize is the most output one response carries.
const consoleReadSize = 32 << 10

// Console serves a console session as a bidirectional stream of bytes: the
// first message names the Machine (and may carry input), each later message
// carries input, and each response carries output. The session ends, and the
// call with it, when the client ends its side of the stream, when the Machine's
// VMM ends the console, or when either side fails.
func (s *Server) Console(stream grpc.BidiStreamingServer[v1alpha2.ConsoleRequest, v1alpha2.ConsoleResponse]) error {
	first, err := stream.Recv()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return rejected("the stream is empty: the first message names the Machine")
		}
		return err
	}
	id, err := target(first.GetMachineId())
	if err != nil {
		return err
	}
	session, err := s.machines.Console(stream.Context(), id)
	if err != nil {
		return status(err)
	}
	defer session.Close()

	// The result is sent before the session is closed, so the loop below that
	// reads it after the session ends never misses it.
	inputEnded := make(chan error, 1)
	go func() {
		input := first.GetInput()
		for {
			if len(input) > 0 {
				if _, err := session.Write(input); err != nil {
					inputEnded <- nil // the session has ended: the read side says why
					_ = session.Close()
					return
				}
			}
			msg, err := stream.Recv()
			if err != nil {
				if errors.Is(err, io.EOF) {
					err = nil
				}
				inputEnded <- err
				_ = session.Close()
				return
			}
			input = msg.GetInput()
		}
	}()

	buf := make([]byte, consoleReadSize)
	for {
		n, readErr := session.Read(buf)
		if n > 0 {
			if err := stream.Send(&v1alpha2.ConsoleResponse{Output: buf[:n]}); err != nil {
				return err
			}
		}
		if readErr == nil {
			continue
		}
		select {
		case err := <-inputEnded:
			return err
		default:
		}
		if errors.Is(readErr, io.EOF) || stream.Context().Err() != nil {
			return nil
		}
		return status(readErr)
	}
}

func (s *Server) PatchFile(ctx context.Context, req *v1alpha2.PatchFileRequest) (*v1alpha2.PatchFileResponse, error) {
	id, err := target(req.GetMachineId())
	if err != nil {
		return nil, err
	}
	if err := s.machines.PatchFile(ctx, id, req.GetPath(), req.GetBaseDigest(), req.GetExpectedDigest(), req.GetPatch()); err != nil {
		return nil, status(err)
	}
	return &v1alpha2.PatchFileResponse{}, nil
}

var entryKinds = map[machine.EntryKind]v1alpha2.EntryKind{
	machine.EntryFile:      v1alpha2.EntryKind_ENTRY_KIND_FILE,
	machine.EntryDirectory: v1alpha2.EntryKind_ENTRY_KIND_DIRECTORY,
	machine.EntrySymlink:   v1alpha2.EntryKind_ENTRY_KIND_SYMLINK,
	machine.EntryOther:     v1alpha2.EntryKind_ENTRY_KIND_OTHER,
}

func (s *Server) ListFiles(req *v1alpha2.ListFilesRequest, stream grpc.ServerStreamingServer[v1alpha2.ListFilesResponse]) error {
	id, err := target(req.GetMachineId())
	if err != nil {
		return err
	}
	send := func(entries []machine.FileEntry) error {
		wire := make([]*v1alpha2.FileEntry, 0, len(entries))
		for _, e := range entries {
			wire = append(wire, &v1alpha2.FileEntry{Path: e.Path, Kind: entryKinds[e.Kind], Size: e.Size})
		}
		return stream.Send(&v1alpha2.ListFilesResponse{Entries: wire})
	}
	return status(s.machines.ListFiles(stream.Context(), id, req.GetPath(), send))
}

func (s *Server) SearchFiles(req *v1alpha2.SearchFilesRequest, stream grpc.ServerStreamingServer[v1alpha2.SearchFilesResponse]) error {
	id, err := target(req.GetMachineId())
	if err != nil {
		return err
	}
	send := func(paths []string) error { return stream.Send(&v1alpha2.SearchFilesResponse{Matches: paths}) }
	return status(s.machines.SearchFiles(stream.Context(), id, req.GetPath(), req.GetPattern(), send))
}
