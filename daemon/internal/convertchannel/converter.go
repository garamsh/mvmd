// Package convertchannel is the image.Converter adapter: a client of the
// converter's gRPC contract, reaching the converter in its microVM over the
// byte stream the Driver opens to the VM's vsock port (image.md §Conversion,
// ADR 0017).
package convertchannel

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	convertv1alpha1 "github.com/garamsh/mvmd/daemon/internal/convertapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// chunkBytes is the most bytes of a layer one Data message carries: the
// contract's bound (ADR 0017).
const chunkBytes = 1 << 20

// Connector opens a byte stream to a VM's port. machine.Driver is one:
// whatever the VMM needs before the first byte is the Driver's.
type Connector interface {
	GuestConn(ctx context.Context, id machine.MachineID, port uint32) (net.Conn, error)
}

// Converter is an image.Converter over the converter's gRPC contract. It
// holds no connection: a conversion opens one through the Connector, for the
// VM the call names, and closes it.
//
// A conversion waits for the converter to listen: the VM is started, not
// ready, when the call begins, so a dial the guest refuses is tried again
// until the call's context ends. Only a conversion that never got a stream
// waits so; one whose connection breaks is not repeated.
//
// A conversion is not repeated when its connection breaks: the converter
// serves one conversion and refuses a second call, so the VM is not reused
// after a break, and the pull fails and starts over with a new VM.
//
// The adapter puts no time limit of its own on a conversion: the pull bounds
// it with the node's conversion limit.
type Converter struct {
	conn Connector
}

var _ image.Converter = (*Converter)(nil)

// New returns a Converter that reaches each converter VM through conn.
func New(conn Connector) *Converter { return &Converter{conn: conn} }

// reconnect is the client's backoff between dials. A refused dial costs one
// local socket exchange, and a guest boots in seconds, so the longest wait is
// one second: that is all a listening converter waits to be found, against a
// conversion limit of minutes.
var reconnect = backoff.Config{
	BaseDelay:  100 * time.Millisecond,
	Multiplier: 1.6,
	Jitter:     0.2,
	MaxDelay:   time.Second,
}

// stepOf maps the contract's enum onto the port's steps; a value the daemon
// has no step for is not mapped.
var stepOf = map[convertv1alpha1.FailureStep]image.ConversionStep{
	convertv1alpha1.FailureStep_FAILURE_STEP_START:      image.StepStart,
	convertv1alpha1.FailureStep_FAILURE_STEP_LAYER_TYPE: image.StepLayerType,
	convertv1alpha1.FailureStep_FAILURE_STEP_STREAM:     image.StepStreamOrder,
	convertv1alpha1.FailureStep_FAILURE_STEP_NAMES:      image.StepNames,
	convertv1alpha1.FailureStep_FAILURE_STEP_NAME_LIMIT: image.StepNameLimit,
	convertv1alpha1.FailureStep_FAILURE_STEP_CONVERT:    image.StepConvert,
	convertv1alpha1.FailureStep_FAILURE_STEP_OUTPUT:     image.StepOutput,
}

// unknownStepError is a failure at a step the contract's enum holds and the
// daemon has no step for: opaque, since no step of the port describes it.
type unknownStepError struct {
	step    convertv1alpha1.FailureStep
	message string
}

func (e *unknownStepError) Error() string {
	return fmt.Sprintf("the converter failed at an unknown step %s: %s", e.step, e.message)
}

// Convert implements image.Converter. A conversion the converter reports as
// failed is an *image.ConversionFailure.
func (c *Converter) Convert(ctx context.Context, id machine.MachineID, job image.ConversionJob) (uint64, error) {
	cc, err := grpc.NewClient("passthrough:///converter",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(dial context.Context, _ string) (net.Conn, error) {
			return c.conn.GuestConn(dial, id, image.ConverterPort)
		}),
		grpc.WithConnectParams(grpc.ConnectParams{Backoff: reconnect}))
	if err != nil {
		return 0, err
	}
	defer func() { _ = cc.Close() }()

	length, err := converse(ctx, convertv1alpha1.NewConvertServiceClient(cc), job)
	return length, mapError(ctx, err)
}

// converse streams the job and reads the answer.
func converse(ctx context.Context, client convertv1alpha1.ConvertServiceClient, job image.ConversionJob) (uint64, error) {
	// Wait-for-ready holds the call until the converter listens, and ends it
	// with the context.
	stream, err := client.Convert(ctx, grpc.WaitForReady(true))
	if err != nil {
		return 0, &notListeningError{err}
	}
	// Send reports only io.EOF once the converter has answered, and the
	// answer, a failure or a status, is on CloseAndRecv.
	if err := send(stream, job); err != nil && !errors.Is(err, io.EOF) {
		return 0, err
	}
	resp, err := stream.CloseAndRecv()
	if err != nil {
		return 0, err
	}
	switch r := resp.GetResult().(type) {
	case *convertv1alpha1.ConvertResponse_Converted:
		return r.Converted.GetLength(), nil
	case *convertv1alpha1.ConvertResponse_Failure:
		step, known := stepOf[r.Failure.GetStep()]
		if !known {
			return 0, &unknownStepError{step: r.Failure.GetStep(), message: r.Failure.GetMessage()}
		}
		return 0, &image.ConversionFailure{Step: step, Message: r.Failure.GetMessage()}
	default:
		return 0, errors.New("the converter answered neither a length nor a failure")
	}
}

// notListeningError is a failure to open the call: the converter never
// listened before the context ended, or the context ended another way.
type notListeningError struct{ err error }

func (e *notListeningError) Error() string { return e.err.Error() }
func (e *notListeningError) Unwrap() error { return e.err }

// readError is a failure to read a staged layer, which no status describes.
type readError struct{ err error }

func (e *readError) Error() string { return e.err.Error() }
func (e *readError) Unwrap() error { return e.err }

type convertStream = grpc.ClientStreamingClient[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]

// send writes the Start, then each layer twice, a names pass and a content
// pass, in manifest order.
func send(stream convertStream, job image.ConversionJob) error {
	types := make([]string, len(job.Layers))
	for i, l := range job.Layers {
		types[i] = l.MediaType
	}
	start := &convertv1alpha1.Start{
		FilesystemUuid:    job.FilesystemUUID,
		LayerMediaTypes:   types,
		NameSetLimitBytes: job.NameSetBytes,
	}
	if err := stream.Send(&convertv1alpha1.ConvertRequest{Message: &convertv1alpha1.ConvertRequest_Start{Start: start}}); err != nil {
		return err
	}
	buf := make([]byte, chunkBytes)
	for i, layer := range job.Layers {
		for _, kind := range []convertv1alpha1.PassKind{convertv1alpha1.PassKind_PASS_KIND_NAMES, convertv1alpha1.PassKind_PASS_KIND_CONTENT} {
			if err := pass(stream, buf, uint32(i), kind, layer.Path); err != nil {
				return err
			}
		}
	}
	return nil
}

// pass sends one Pass and the layer's file as Data messages.
func pass(stream convertStream, buf []byte, layer uint32, kind convertv1alpha1.PassKind, path string) error {
	p := &convertv1alpha1.Pass{Layer: layer, Kind: kind}
	if err := stream.Send(&convertv1alpha1.ConvertRequest{Message: &convertv1alpha1.ConvertRequest_Pass{Pass: p}}); err != nil {
		return err
	}
	f, err := os.Open(path)
	if err != nil {
		return &readError{fmt.Errorf("open layer %d: %w", layer, err)}
	}
	defer func() { _ = f.Close() }()
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			if err := stream.Send(&convertv1alpha1.ConvertRequest{Message: &convertv1alpha1.ConvertRequest_Data{Data: buf[:n]}}); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return &readError{fmt.Errorf("read layer %d: %w", layer, readErr)}
		}
	}
}

// mapError turns what the converter or the connection answered into the
// errors the Converter port promises. The converter reports a conversion that
// failed as a message in its answer (convert.proto), which converse has
// already turned into an error of its own, and its contract carries no status
// details: a status means the call itself broke, so it maps by its code and
// never by its message, which is for people.
func mapError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	var failure *image.ConversionFailure
	var unknown *unknownStepError
	var read *readError
	var notListening *notListeningError
	if errors.As(err, &notListening) && errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("%w: the converter did not listen before the conversion's time ended: %w",
			machine.ErrGuestUnreachable, ctx.Err())
	}
	if errors.As(err, &failure) || errors.As(err, &unknown) || errors.As(err, &read) {
		return err
	}
	st, ok := status.FromError(err)
	if !ok {
		return err
	}
	msg := st.Message()
	switch st.Code() {
	case codes.Unavailable:
		return fmt.Errorf("%w: %s", machine.ErrGuestUnreachable, msg)
	case codes.Canceled:
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %s", ctx.Err(), msg)
		}
	case codes.DeadlineExceeded:
		return fmt.Errorf("%w: %s", context.DeadlineExceeded, msg)
	case codes.AlreadyExists:
		return fmt.Errorf("the converter already served a conversion: %s", msg)
	}
	return fmt.Errorf("the converter answered %s: %s", st.Code(), msg)
}
