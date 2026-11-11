package v1alpha2_test

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"

	"github.com/garamsh/mvmd/lib/runtimeapi/v1alpha2"
)

const execOutput = "0123456789abcdefghij"

type outputStream = grpc.ServerStreamingServer[v1alpha2.ReadExecOutputResponse]

// outputServer holds execOutput and answers each open of ReadExecOutput with
// the behaviour attempt gives for it; without one it sends the rest of the
// output and the record that ends it.
type outputServer struct {
	v1alpha2.UnimplementedRuntimeServiceServer
	attempt func(open int, after uint64, stream outputStream) error

	mu    sync.Mutex
	opens []uint64
}

func (s *outputServer) ReadExecOutput(req *v1alpha2.ReadExecOutputRequest, stream outputStream) error {
	s.mu.Lock()
	open := len(s.opens)
	s.opens = append(s.opens, req.GetAfterOffset())
	s.mu.Unlock()
	if s.attempt != nil {
		return s.attempt(open, req.GetAfterOffset(), stream)
	}
	return sendRest(stream, req.GetAfterOffset())
}

func (s *outputServer) afterOffsets() []uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]uint64(nil), s.opens...)
}

// sendOutput sends execOutput from the offset after to the offset end, in
// chunks of at most four bytes.
func sendOutput(stream outputStream, after uint64, end int) error {
	for offset := int(after); offset < end; offset += 4 {
		data := execOutput[offset:min(offset+4, end)]
		part := &v1alpha2.ReadExecOutputResponse{Part: &v1alpha2.ReadExecOutputResponse_Chunk{
			Chunk: &v1alpha2.OutputChunk{Stream: v1alpha2.OutputStream_OUTPUT_STREAM_STDOUT, Offset: uint64(offset), Data: []byte(data)},
		}}
		if err := stream.Send(part); err != nil {
			return err
		}
	}
	return nil
}

// sendRest sends the output after the offset and the record that ends it.
func sendRest(stream outputStream, after uint64) error {
	if err := sendOutput(stream, after, len(execOutput)); err != nil {
		return err
	}
	return stream.Send(&v1alpha2.ReadExecOutputResponse{Part: &v1alpha2.ReadExecOutputResponse_Record{
		Record: &v1alpha2.ExecRecord{ExecId: "e", Status: v1alpha2.ExecStatus_EXEC_STATUS_EXITED, ExitCode: 3},
	}})
}

func serveOutput(t *testing.T, s *outputServer) v1alpha2.RuntimeServiceClient {
	t.Helper()
	lis := bufconn.Listen(1 << 20)
	srv := grpc.NewServer()
	v1alpha2.RegisterRuntimeServiceServer(srv, s)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) { return lis.DialContext(ctx) }),
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return v1alpha2.NewRuntimeServiceClient(conn)
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// collect returns a sink that gathers the bytes of the chunks and fails the
// test on a chunk that does not start where the last one ended.
func collect(t *testing.T, from uint64) (sink func(*v1alpha2.OutputChunk) error, got func() string) {
	var out []byte
	next := from
	return func(c *v1alpha2.OutputChunk) error {
		require.Equal(t, next, c.GetOffset(), "a chunk starts where the last one ended")
		out = append(out, c.GetData()...)
		next += uint64(len(c.GetData()))
		return nil
	}, func() string { return string(out) }
}

func TestReadExecOutput_ReadsToTheEnd(t *testing.T) {
	s := &outputServer{}
	sink, got := collect(t, 0)

	record, err := v1alpha2.ReadExecOutput(testContext(t), serveOutput(t, s), "m", "e", 0, sink)

	require.NoError(t, err)
	assert.Equal(t, execOutput, got())
	assert.Equal(t, v1alpha2.ExecStatus_EXEC_STATUS_EXITED, record.GetStatus())
	assert.Equal(t, int32(3), record.GetExitCode())
	assert.Equal(t, []uint64{0}, s.afterOffsets(), "one stream, opened at the start")
}

func TestReadExecOutput_StartsAtTheOffsetGiven(t *testing.T) {
	s := &outputServer{}
	sink, got := collect(t, 8)

	_, err := v1alpha2.ReadExecOutput(testContext(t), serveOutput(t, s), "m", "e", 8, sink)

	require.NoError(t, err)
	assert.Equal(t, execOutput[8:], got())
	assert.Equal(t, []uint64{8}, s.afterOffsets())
}

// A stream that breaks is opened again after the end of the last chunk passed
// on, and the sink sees every byte once: the first stream's last chunk is
// three bytes long, so an offset taken from a count of chunks would repeat or
// skip bytes.
func TestReadExecOutput_ResumesAfterABrokenStream(t *testing.T) {
	tests := []struct {
		name   string
		ending error
	}{
		{"Unavailable", status.Error(codes.Unavailable, "the connection reset")},
		{"ended without a record", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &outputServer{}
			s.attempt = func(open int, after uint64, stream outputStream) error {
				if open > 0 {
					return sendRest(stream, after)
				}
				if err := sendOutput(stream, after, 11); err != nil {
					return err
				}
				return tt.ending
			}
			sink, got := collect(t, 0)

			record, err := v1alpha2.ReadExecOutput(testContext(t), serveOutput(t, s), "m", "e", 0, sink)

			require.NoError(t, err)
			assert.Equal(t, execOutput, got(), "no byte repeated or lost")
			assert.Equal(t, int32(3), record.GetExitCode())
			assert.Equal(t, []uint64{0, 11}, s.afterOffsets(), "the second stream resumes after the last byte delivered")
		})
	}
}

// An error that carries an outcome kind is the call's answer and is returned
// at once; the control is the Unavailable of the test above, which is read
// again.
func TestReadExecOutput_DoesNotRetryAnOutcomeKind(t *testing.T) {
	withKind, err := status.New(codes.Unavailable, "refused").WithDetails(&v1alpha2.Failure{Kind: v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED, Reason: "refused"})
	require.NoError(t, err)
	tests := []struct {
		name string
		err  error
		kind v1alpha2.OutcomeKind
	}{
		{"not found", v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND, "no such exec"), v1alpha2.OutcomeKind_OUTCOME_KIND_NOT_FOUND},
		{"precondition failed", v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED, "not running"), v1alpha2.OutcomeKind_OUTCOME_KIND_PRECONDITION_FAILED},
		{"rejected", v1alpha2.NewError(v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED, "exec_id is empty"), v1alpha2.OutcomeKind_OUTCOME_KIND_REJECTED},
		{"Unavailable carrying a kind", withKind.Err(), v1alpha2.OutcomeKind_OUTCOME_KIND_FAILED},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &outputServer{attempt: func(int, uint64, outputStream) error { return tt.err }}
			sink, _ := collect(t, 0)

			record, err := v1alpha2.ReadExecOutput(testContext(t), serveOutput(t, s), "m", "e", 0, sink)

			require.Error(t, err)
			assert.Nil(t, record)
			assert.Equal(t, tt.kind, v1alpha2.KindOf(err))
			assert.Equal(t, []uint64{0}, s.afterOffsets(), "opened once")
		})
	}
}

// A resume before the earliest offset still held is returned with that offset
// and the gap is not skipped: the sink has only what came before it.
func TestReadExecOutput_ReturnsOutputGone(t *testing.T) {
	s := &outputServer{}
	s.attempt = func(open int, after uint64, stream outputStream) error {
		if open > 0 {
			return v1alpha2.NewOutputGone("the output before offset 16 is no longer held", 16)
		}
		if err := sendOutput(stream, after, 8); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, "the connection reset")
	}
	sink, got := collect(t, 0)

	record, err := v1alpha2.ReadExecOutput(testContext(t), serveOutput(t, s), "m", "e", 0, sink)

	require.Error(t, err)
	assert.Nil(t, record)
	earliest, ok := v1alpha2.EarliestOffset(err)
	require.True(t, ok, "the error carries the earliest offset held")
	assert.Equal(t, uint64(16), earliest)
	assert.Equal(t, execOutput[:8], got(), "the bytes up to the gap, none after it")
	assert.Equal(t, []uint64{0, 8}, s.afterOffsets(), "the resume was tried once")
}

// A sink's error stops the read and comes back as it is, even one that looks
// like a broken transport.
func TestReadExecOutput_ReturnsTheSinksError(t *testing.T) {
	tests := []struct {
		name string
		stop error
	}{
		{"a plain error", errors.New("enough")},
		{"Unavailable", status.Error(codes.Unavailable, "the sink's own")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &outputServer{}

			record, err := v1alpha2.ReadExecOutput(testContext(t), serveOutput(t, s), "m", "e", 0, func(*v1alpha2.OutputChunk) error { return tt.stop })

			assert.Nil(t, record)
			assert.Equal(t, tt.stop, err)
			assert.Equal(t, []uint64{0}, s.afterOffsets(), "opened once")
		})
	}
}

// Retries are bounded by the caller's context.
func TestReadExecOutput_StopsWhenTheContextEnds(t *testing.T) {
	s := &outputServer{attempt: func(int, uint64, outputStream) error {
		return status.Error(codes.Unavailable, "down")
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	sink, _ := collect(t, 0)

	record, err := v1alpha2.ReadExecOutput(ctx, serveOutput(t, s), "m", "e", 0, sink)

	assert.Nil(t, record)
	require.Error(t, err)
	assert.True(t, errors.Is(err, context.DeadlineExceeded) || status.Code(err) == codes.DeadlineExceeded, "got %v", err)
	assert.Greater(t, len(s.afterOffsets()), 1, "it retried until the context ended")
}
