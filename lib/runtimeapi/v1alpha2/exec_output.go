package v1alpha2

import (
	"context"
	"errors"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	firstRetryWait   = 50 * time.Millisecond
	longestRetryWait = time.Second
)

// ReadExecOutput reads the output of an exec from the offset after to the
// record that ends it, passes each chunk to sink in order and returns the
// record. A stream that ends without the record or fails with Unavailable is
// opened again after the end of the last chunk passed on, with a wait that
// doubles from 50 ms to one second, until ctx ends. Any other error is
// returned at once: one that carries an outcome kind, an OutputGone (see
// EarliestOffset), the error of sink, and the error of ctx. sink returning an
// error stops the read and is returned as it is.
func ReadExecOutput(ctx context.Context, client RuntimeServiceClient, machineID, execID string, after uint64, sink func(*OutputChunk) error) (*ExecRecord, error) {
	next := after
	wait := firstRetryWait
	for {
		stream, err := client.ReadExecOutput(ctx, &ReadExecOutputRequest{MachineId: machineID, ExecId: execID, AfterOffset: next})
		var record *ExecRecord
		var passed bool
		if err == nil {
			record, passed, err = drainExecOutput(stream, sink, &next)
		} else if brokenStream(err) {
			err = nil
		}
		if err != nil {
			return nil, err
		}
		if record != nil {
			return record, nil
		}
		if passed {
			wait = firstRetryWait
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(wait):
		}
		wait = min(2*wait, longestRetryWait)
	}
}

// drainExecOutput passes the chunks of the stream to sink until the record, an
// error that ends the read or the stream's end. A stream that ended or broke
// is a nil record and a nil error. next moves to the end of each chunk passed
// on, and passed reports whether one was.
func drainExecOutput(stream grpc.ServerStreamingClient[ReadExecOutputResponse], sink func(*OutputChunk) error, next *uint64) (record *ExecRecord, passed bool, err error) {
	for {
		part, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			return nil, passed, nil
		}
		if brokenStream(err) {
			return nil, passed, nil
		}
		if err != nil {
			return nil, passed, err
		}
		if chunk := part.GetChunk(); chunk != nil {
			if err := sink(chunk); err != nil {
				return nil, passed, err
			}
			*next = chunk.GetOffset() + uint64(len(chunk.GetData()))
			passed = true
		}
		if record := part.GetRecord(); record != nil {
			return record, passed, nil
		}
	}
}

// brokenStream reports whether err is a transport that broke: Unavailable
// with no outcome kind.
func brokenStream(err error) bool {
	return status.Code(err) == codes.Unavailable && KindOf(err) == OutcomeKind_OUTCOME_KIND_UNSPECIFIED
}
