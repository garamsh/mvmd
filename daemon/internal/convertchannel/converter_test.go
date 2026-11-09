package convertchannel_test

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/garamsh/mvmd/daemon/internal/convertapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/convertchannel"
	"github.com/garamsh/mvmd/daemon/internal/image"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

const (
	tarType = "application/vnd.oci.image.layer.v1.tar"
	gzType  = "application/vnd.oci.image.layer.v1.tar+gzip"
)

// fakeConverter is a converter whose answer a test chooses. It keeps the
// requests of the call it served.
type fakeConverter struct {
	convertv1alpha1.UnimplementedConvertServiceServer
	answer func(stream grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error

	mu   sync.Mutex
	seen []*convertv1alpha1.ConvertRequest
}

// readAll reads the stream to its end and answers with the length.
func (f *fakeConverter) readAll(length uint64) func(grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
	return func(stream grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
		for {
			msg, err := stream.Recv()
			if err != nil {
				break
			}
			f.mu.Lock()
			f.seen = append(f.seen, msg)
			f.mu.Unlock()
		}
		return stream.SendAndClose(&convertv1alpha1.ConvertResponse{Result: &convertv1alpha1.ConvertResponse_Converted{Converted: &convertv1alpha1.Converted{Length: length}}})
	}
}

func (f *fakeConverter) Convert(stream grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
	return f.answer(stream)
}

// dialer is a Connector to a server on a Unix socket, which records the VM and
// the port it was asked for.
type dialer struct {
	path string
	mu   sync.Mutex
	ids  []machine.MachineID
	port []uint32
}

func (d *dialer) GuestConn(ctx context.Context, id machine.MachineID, port uint32) (net.Conn, error) {
	d.mu.Lock()
	d.ids = append(d.ids, id)
	d.port = append(d.port, port)
	d.mu.Unlock()
	var nd net.Dialer
	return nd.DialContext(ctx, "unix", d.path)
}

// serve starts the fake on a socket and returns the adapter over it.
func serve(t *testing.T, f *fakeConverter) (*convertchannel.Converter, *dialer) {
	t.Helper()
	dir, err := os.MkdirTemp("", "conv")
	require.NoError(t, err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "c.sock")
	lis, err := net.Listen("unix", path)
	require.NoError(t, err)
	srv := grpc.NewServer()
	convertv1alpha1.RegisterConvertServiceServer(srv, f)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(srv.Stop)
	d := &dialer{path: path}
	return convertchannel.New(d), d
}

// layerFile writes a staged layer of the given content and returns it.
func layerFile(t *testing.T, mediaType string, content []byte) image.StagedLayer {
	t.Helper()
	path := filepath.Join(t.TempDir(), "layer")
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return image.StagedLayer{Path: path, MediaType: mediaType}
}

func jobOf(layers ...image.StagedLayer) image.ConversionJob {
	return image.ConversionJob{FilesystemUUID: "11111111-2222-3333-4444-555555555555", Layers: layers, NameSetBytes: 4096}
}

func TestConvert_StreamsTheStartThenEachLayerInTwoPasses(t *testing.T) {
	f := &fakeConverter{}
	f.answer = f.readAll(777)
	c, d := serve(t, f)
	big := bytes.Repeat([]byte("x"), 2<<20+5) // more than two Data messages
	first := layerFile(t, tarType, []byte("first"))
	second := layerFile(t, gzType, big)

	length, err := c.Convert(context.Background(), "convert-abc", jobOf(first, second))
	require.NoError(t, err)
	assert.Equal(t, uint64(777), length)

	// It reached the VM it was asked for, on the converter's port.
	assert.Equal(t, []machine.MachineID{"convert-abc"}, d.ids)
	assert.Equal(t, []uint32{1025}, d.port)

	// The first message is the Start, with the job's fields in manifest order.
	require.NotEmpty(t, f.seen)
	start := f.seen[0].GetStart()
	require.NotNil(t, start, "the first message is a Start")
	assert.Equal(t, "11111111-2222-3333-4444-555555555555", start.GetFilesystemUuid())
	assert.Equal(t, []string{tarType, gzType}, start.GetLayerMediaTypes())
	assert.Equal(t, uint64(4096), start.GetNameSetLimitBytes())

	// Then each layer's names pass and content pass, each with the layer's
	// bytes whole, in Data messages of at most 1 MiB.
	type passed struct {
		layer uint32
		kind  convertv1alpha1.PassKind
		data  []byte
	}
	var passes []*passed
	for _, m := range f.seen[1:] {
		switch p := m.GetMessage().(type) {
		case *convertv1alpha1.ConvertRequest_Pass:
			passes = append(passes, &passed{layer: p.Pass.GetLayer(), kind: p.Pass.GetKind()})
		case *convertv1alpha1.ConvertRequest_Data:
			require.NotEmpty(t, passes, "Data before any Pass")
			assert.LessOrEqual(t, len(p.Data), 1<<20)
			last := passes[len(passes)-1]
			last.data = append(last.data, p.Data...)
		default:
			t.Fatalf("unexpected message %v", m)
		}
	}
	require.Len(t, passes, 4)
	names, content := convertv1alpha1.PassKind_PASS_KIND_NAMES, convertv1alpha1.PassKind_PASS_KIND_CONTENT
	want := []struct {
		layer uint32
		kind  convertv1alpha1.PassKind
		data  []byte
	}{{0, names, []byte("first")}, {0, content, []byte("first")}, {1, names, big}, {1, content, big}}
	for i, w := range want {
		assert.Equal(t, w.layer, passes[i].layer, "pass %d", i)
		assert.Equal(t, w.kind, passes[i].kind, "pass %d", i)
		assert.True(t, bytes.Equal(w.data, passes[i].data), "pass %d carries the layer's bytes", i)
	}
}

func fail(step convertv1alpha1.FailureStep, message string) func(grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
	return func(stream grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
		for {
			if _, err := stream.Recv(); err != nil {
				break
			}
		}
		return stream.SendAndClose(&convertv1alpha1.ConvertResponse{Result: &convertv1alpha1.ConvertResponse_Failure{Failure: &convertv1alpha1.Failure{Step: step, Message: message}}})
	}
}

// Each failure step the contract names has a row; the message each carries is
// the text of another step, so a mapping that read it would be wrong.
func TestConvert_AFailureMapsByItsStepAndNotByItsMessage(t *testing.T) {
	rows := []struct {
		step convertv1alpha1.FailureStep
		want string
	}{
		{convertv1alpha1.FailureStep_FAILURE_STEP_START, "start"},
		{convertv1alpha1.FailureStep_FAILURE_STEP_LAYER_TYPE, "layer type"},
		{convertv1alpha1.FailureStep_FAILURE_STEP_STREAM, "stream order"},
		{convertv1alpha1.FailureStep_FAILURE_STEP_NAMES, "names"},
		{convertv1alpha1.FailureStep_FAILURE_STEP_NAME_LIMIT, "name limit"},
		{convertv1alpha1.FailureStep_FAILURE_STEP_CONVERT, "convert"},
		{convertv1alpha1.FailureStep_FAILURE_STEP_OUTPUT, "output"},
		// A control with no step the daemon knows: the enum's own name.
		{convertv1alpha1.FailureStep_FAILURE_STEP_UNSPECIFIED, "FAILURE_STEP_UNSPECIFIED"},
		{convertv1alpha1.FailureStep(99), "99"},
	}
	for _, r := range rows {
		t.Run(r.want, func(t *testing.T) {
			f := &fakeConverter{answer: fail(r.step, "the output step failed")}
			c, _ := serve(t, f)
			_, err := c.Convert(context.Background(), "m", jobOf(layerFile(t, tarType, []byte("a"))))
			var stepErr *convertchannel.StepError
			require.ErrorAs(t, err, &stepErr)
			assert.Equal(t, r.want, stepErr.Step)
			assert.Equal(t, "the output step failed", stepErr.Message)
			assert.NotErrorIs(t, err, machine.ErrGuestUnreachable)
		})
	}

	// Control: an answer with a length is no error.
	f := &fakeConverter{}
	f.answer = f.readAll(1)
	c, _ := serve(t, f)
	_, err := c.Convert(context.Background(), "m", jobOf(layerFile(t, tarType, []byte("a"))))
	require.NoError(t, err)
}

// A converter that has failed answers before it has read every layer; the
// client is still sending, and what it reports is the failure.
func TestConvert_AFailureAnsweredEarlyIsReportedWhileTheClientStillSends(t *testing.T) {
	f := &fakeConverter{answer: func(stream grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
		_, _ = stream.Recv() // the Start only
		return stream.SendAndClose(&convertv1alpha1.ConvertResponse{Result: &convertv1alpha1.ConvertResponse_Failure{Failure: &convertv1alpha1.Failure{Step: convertv1alpha1.FailureStep_FAILURE_STEP_LAYER_TYPE, Message: "refused"}}})
	}}
	c, _ := serve(t, f)
	big := layerFile(t, tarType, bytes.Repeat([]byte("x"), 32<<20))

	_, err := c.Convert(context.Background(), "m", jobOf(big))
	var stepErr *convertchannel.StepError
	require.ErrorAs(t, err, &stepErr)
	assert.Equal(t, "layer type", stepErr.Step)
}

func TestConvert_AStatusMapsByItsCodeAndNotByItsMessage(t *testing.T) {
	rows := []struct {
		name    string
		code    codes.Code
		message string
		check   func(t *testing.T, err error)
	}{
		{"unavailable is an unreachable converter", codes.Unavailable, "gone", func(t *testing.T, err error) {
			assert.ErrorIs(t, err, machine.ErrGuestUnreachable)
		}},
		{"deadline exceeded", codes.DeadlineExceeded, "slow", func(t *testing.T, err error) {
			assert.ErrorIs(t, err, context.DeadlineExceeded)
		}},
		{"already exists is a second call", codes.AlreadyExists, "x", func(t *testing.T, err error) {
			assert.ErrorContains(t, err, "already served a conversion")
			assert.NotErrorIs(t, err, machine.ErrGuestUnreachable)
		}},
		// Control: a code with no row, whose message says what another row's
		// code would, is neither unreachable nor a served conversion.
		{"internal with another code's words", codes.Internal, "unavailable: already exists", func(t *testing.T, err error) {
			require.Error(t, err)
			assert.NotErrorIs(t, err, machine.ErrGuestUnreachable)
			assert.NotContains(t, err.Error(), "already served a conversion")
			assert.ErrorContains(t, err, "Internal")
		}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := &fakeConverter{answer: func(grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
				return status.Error(r.code, r.message)
			}}
			c, _ := serve(t, f)
			_, err := c.Convert(context.Background(), "m", jobOf(layerFile(t, tarType, []byte("a"))))
			require.Error(t, err)
			r.check(t, err)
		})
	}
}

func TestConvert_ACanceledContextIsCanceled(t *testing.T) {
	started := make(chan struct{})
	f := &fakeConverter{answer: func(stream grpc.ClientStreamingServer[convertv1alpha1.ConvertRequest, convertv1alpha1.ConvertResponse]) error {
		close(started)
		<-stream.Context().Done()
		return stream.Context().Err()
	}}
	c, _ := serve(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { <-started; cancel() }()

	_, err := c.Convert(ctx, "m", jobOf(layerFile(t, tarType, []byte("a"))))
	require.ErrorIs(t, err, context.Canceled)
}

type failingConnector struct{ err error }

func (c failingConnector) GuestConn(context.Context, machine.MachineID, uint32) (net.Conn, error) {
	return nil, c.err
}

func TestConvert_AnUnreachableVMIsUnreachable(t *testing.T) {
	c := convertchannel.New(failingConnector{machine.ErrGuestUnreachable})
	_, err := c.Convert(context.Background(), "m", jobOf(layerFile(t, tarType, []byte("a"))))
	require.ErrorIs(t, err, machine.ErrGuestUnreachable)
}

func TestConvert_AMissingLayerFileIsAReadFailureAndNotAStatus(t *testing.T) {
	f := &fakeConverter{}
	f.answer = f.readAll(1)
	c, _ := serve(t, f)
	gone := image.StagedLayer{Path: filepath.Join(t.TempDir(), "absent"), MediaType: tarType}

	_, err := c.Convert(context.Background(), "m", jobOf(gone))
	require.ErrorIs(t, err, os.ErrNotExist)
	assert.NotErrorIs(t, err, machine.ErrGuestUnreachable)
	var stepErr *convertchannel.StepError
	assert.False(t, errors.As(err, &stepErr))
}
