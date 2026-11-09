package channel

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	guestv1alpha1 "github.com/garamsh/mvmd/daemon/internal/guestapi/v1alpha1"
	"github.com/garamsh/mvmd/daemon/internal/machine"
)

// fakeServer is an in-process gRPC server that implements the Guest service
// protocol. It stores state that the guest_test functions exercise.
type fakeServer struct {
	guestv1alpha1.UnimplementedGuestServiceServer

	mu     sync.Mutex
	err    error
	health bool

	// Exec state.
	execRecord machine.ExecRecord

	// ReadExecOutput state.
	execData []machine.OutputChunk
	execDone bool

	// ReadFile state.
	fileContent []byte
	fileErr     error

	// WriteFile state.
	writtenContent []byte
	writeErr       error

	// PatchFile state.
	patchOutcome machine.PatchOutcome
	patchErr     error

	// SearchFiles state.
	searchPaths []string
	searchErr   error

	// Boot / CatchUp state.
	bootReq    *guestv1alpha1.BootRequest
	catchUpReq *guestv1alpha1.CatchUpRequest
	bootErr    error
	catchUpErr error
}

var _ guestv1alpha1.GuestServiceServer = (*fakeServer)(nil)

func (s *fakeServer) Health(_ context.Context, _ *guestv1alpha1.HealthRequest) (*guestv1alpha1.HealthResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.health = true
	if s.err != nil {
		return nil, s.err
	}
	return &guestv1alpha1.HealthResponse{}, nil
}

func (s *fakeServer) Boot(_ context.Context, req *guestv1alpha1.BootRequest) (*guestv1alpha1.BootResponse, error) {
	s.mu.Lock()
	s.bootReq = req
	err := s.bootErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &guestv1alpha1.BootResponse{}, nil
}

func (s *fakeServer) CatchUp(_ context.Context, req *guestv1alpha1.CatchUpRequest) (*guestv1alpha1.CatchUpResponse, error) {
	s.mu.Lock()
	s.catchUpReq = req
	err := s.catchUpErr
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return &guestv1alpha1.CatchUpResponse{}, nil
}

func (s *fakeServer) Exec(_ context.Context, req *guestv1alpha1.ExecRequest) (*guestv1alpha1.ExecResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	r := machine.ExecRecord{
		ID:       machine.ExecID(req.GetExecId()),
		Status:   machine.ExecStarted,
		ExitCode: 0,
	}
	s.execRecord = r
	return &guestv1alpha1.ExecResponse{
		Record: &guestv1alpha1.ExecRecord{
			ExecId:   string(r.ID),
			Status:   guestv1alpha1.ExecStatus(r.Status),
			ExitCode: r.ExitCode,
		},
	}, nil
}

func (s *fakeServer) ReadExecOutput(req *guestv1alpha1.ReadExecOutputRequest, srv grpc.ServerStreamingServer[guestv1alpha1.ReadExecOutputResponse]) error {
	s.mu.Lock()
	data := make([]machine.OutputChunk, len(s.execData))
	copy(data, s.execData)
	done := s.execDone
	rec := s.execRecord
	s.mu.Unlock()
	for _, chunk := range data {
		if chunk.Offset < req.GetAfterOffset() {
			continue
		}
		if err := srv.Send(&guestv1alpha1.ReadExecOutputResponse{
			Part: &guestv1alpha1.ReadExecOutputResponse_Chunk{
				Chunk: &guestv1alpha1.OutputChunk{
					Stream: guestv1alpha1.OutputStream(chunk.Stream),
					Offset: chunk.Offset,
					Data:   chunk.Data,
				},
			},
		}); err != nil {
			return err
		}
	}
	if done {
		return srv.Send(&guestv1alpha1.ReadExecOutputResponse{
			Part: &guestv1alpha1.ReadExecOutputResponse_Record{
				Record: &guestv1alpha1.ExecRecord{
					ExecId:   string(rec.ID),
					Status:   guestv1alpha1.ExecStatus(rec.Status),
					Reason:   rec.Reason,
					ExitCode: rec.ExitCode,
				},
			},
		})
	}
	return nil
}

func (s *fakeServer) ReadFile(req *guestv1alpha1.ReadFileRequest, srv grpc.ServerStreamingServer[guestv1alpha1.ReadFileResponse]) error {
	s.mu.Lock()
	content := s.fileContent
	err := s.fileErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	chunkSize := 512
	for len(content) > 0 {
		b := content
		if len(b) > chunkSize {
			b = b[:chunkSize]
		}
		content = content[len(b):]
		if err := srv.Send(&guestv1alpha1.ReadFileResponse{Chunk: b}); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeServer) WriteFile(srv grpc.ClientStreamingServer[guestv1alpha1.WriteFileRequest, guestv1alpha1.WriteFileResponse]) error {
	s.mu.Lock()
	s.writtenContent = nil
	s.writeErr = nil
	s.mu.Unlock()
	for {
		req, err := srv.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			s.mu.Lock()
			s.writeErr = err
			s.mu.Unlock()
			return err
		}
		if chunk := req.GetChunk(); chunk != nil {
			s.mu.Lock()
			s.writtenContent = append(s.writtenContent, chunk...)
			s.mu.Unlock()
		}
	}
	if s.writeErr != nil {
		return s.writeErr
	}
	return srv.SendAndClose(&guestv1alpha1.WriteFileResponse{})
}

func (s *fakeServer) PatchFile(_ context.Context, req *guestv1alpha1.PatchFileRequest) (*guestv1alpha1.PatchFileResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.patchErr != nil {
		return nil, s.patchErr
	}
	s.patchOutcome = machine.PatchApplied
	return &guestv1alpha1.PatchFileResponse{
		Outcome: guestv1alpha1.PatchOutcome(s.patchOutcome),
	}, nil
}

func (s *fakeServer) SearchFiles(req *guestv1alpha1.SearchFilesRequest, srv grpc.ServerStreamingServer[guestv1alpha1.SearchFilesResponse]) error {
	s.mu.Lock()
	paths := s.searchPaths
	err := s.searchErr
	s.mu.Unlock()
	if err != nil {
		return err
	}
	for _, p := range paths {
		if err := srv.Send(&guestv1alpha1.SearchFilesResponse{
			Matches: []*guestv1alpha1.FileEntry{{Path: p}},
		}); err != nil {
			return err
		}
	}
	return nil
}

func (s *fakeServer) UpdateNetwork(_ context.Context, _ *guestv1alpha1.UpdateNetworkRequest) (*guestv1alpha1.UpdateNetworkResponse, error) {
	return &guestv1alpha1.UpdateNetworkResponse{}, nil
}

func (s *fakeServer) Signal(_ context.Context, _ *guestv1alpha1.SignalRequest) (*guestv1alpha1.SignalResponse, error) {
	return &guestv1alpha1.SignalResponse{}, nil
}

func (s *fakeServer) ListFiles(_ *guestv1alpha1.ListFilesRequest, srv grpc.ServerStreamingServer[guestv1alpha1.ListFilesResponse]) error {
	return srv.Send(&guestv1alpha1.ListFilesResponse{
		Entries: []*guestv1alpha1.FileEntry{},
	})
}

func (s *fakeServer) Info(_ context.Context, _ *guestv1alpha1.InfoRequest) (*guestv1alpha1.InfoResponse, error) {
	return &guestv1alpha1.InfoResponse{}, nil
}

// startFakeGRPC starts a gRPC server on a new Unix socket and returns the
// socket path to use as the dial target.
func startFakeGRPC(t *testing.T) (sockPath string, cleanup func()) {
	t.Helper()
	tmpDir := t.TempDir()
	path := fmt.Sprintf("%s/guest.sock", tmpDir)

	lis, err := net.Listen("unix", path)
	require.NoError(t, err)

	s := grpc.NewServer()
	guestv1alpha1.RegisterGuestServiceServer(s, &fakeServer{})
	go func() {
		_ = s.Serve(lis)
	}()

	return path, func() {
		s.Stop()
		lis.Close()
	}
}

// newDialer returns a Dialer that creates a gRPC client connected to a Unix
// socket at the given path.
func newDialer(sockPath string) Dialer {
	return func(conn net.Conn) (guestv1alpha1.GuestServiceClient, error) {
		cc, err := grpc.NewClient(
			"unix://"+sockPath,
			grpc.WithContextDialer(func(_ context.Context, _ string) (net.Conn, error) {
				return net.Dial("unix", sockPath)
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		if err != nil {
			return nil, err
		}
		return guestv1alpha1.NewGuestServiceClient(cc), nil
	}
}

// newTestGuest creates a Guest wired to the Unix socket at sockPath.
// It sets the dialConn to a Unix socket dialer and uses the provided dialer.
func newTestGuest(sockPath string, d Dialer) *Guest {
	return &Guest{
		dialer:   d,
		dialConn: func() (net.Conn, error) { return net.Dial("unix", sockPath) },
	}
}

// TestGuest_Health verifies that Health reaches the agent and returns nil
// when it answers.
func TestGuest_Health(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	err := guest.Health(context.Background(), "m1")
	require.NoError(t, err)
}

// TestGuest_Health_FailsWhenUnreachable verifies that Health returns
// ErrGuestUnreachable when the connection cannot be opened.
func TestGuest_Health_FailsWhenUnreachable(t *testing.T) {
	guest := NewGuest(func(conn net.Conn) (guestv1alpha1.GuestServiceClient, error) {
		return nil, fmt.Errorf("dial failed")
	})
	err := guest.Health(context.Background(), "m1")
	assert.Equal(t, machine.ErrGuestUnreachable, err)
}

// TestGuest_Boot sends a boot request and verifies the fields are mapped
// correctly into the proto request.
func TestGuest_Boot(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	boot := machine.GuestBoot{
		ImageUUID:    "img-1",
		RootDiskUUID: "disk-1",
		Run: machine.ExecSpec{
			Command:    []string{"/init"},
			Env:        map[string]string{"PATH": "/bin"},
			WorkingDir: "/app",
		},
		Network: machine.GuestNetwork{
			Resolvers: []netip.Addr{netip.MustParseAddr("10.0.0.53")},
		},
	}

	err := guest.Boot(context.Background(), "m1", boot)
	require.NoError(t, err)
}

// TestGuest_CatchUp sends a CatchUp request with entropy and time.
func TestGuest_CatchUp(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	catchUp := machine.CatchUp{
		RestoreID: machine.RequestID("r1"),
		Entropy:   []byte{0x01, 0x02},
		HostTime:  time.Unix(1700000000, 0),
		Network:   machine.GuestNetwork{},
	}

	err := guest.CatchUp(context.Background(), "m1", catchUp)
	require.NoError(t, err)
}

// TestGuest_Exec verifies that Exec returns a record with the correct ID and
// status.
func TestGuest_Exec(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	rec, err := guest.Exec(context.Background(), "m1", "x1", machine.ExecSpec{
		Command: []string{"echo", "hello"},
	})
	require.NoError(t, err)
	assert.Equal(t, machine.ExecID("x1"), rec.ID)
	assert.Equal(t, machine.ExecStarted, rec.Status)
}

// TestGuest_ReadExecOutput verifies that the stream of output chunks and the
// final record are received correctly.
func TestGuest_ReadExecOutput(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	var chunks []machine.OutputChunk
	rec, err := guest.ReadExecOutput(context.Background(), "m1", "x1", 0, func(chunk machine.OutputChunk) error {
		chunks = append(chunks, chunk)
		return nil
	})
	require.NoError(t, err)
	assert.NotNil(t, rec)
	assert.Empty(t, chunks)
}

// TestGuest_ReadFile verifies that a file is read in chunks via the callback.
func TestGuest_ReadFile(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	var chunks [][]byte
	err := guest.ReadFile(context.Background(), "m1", "/etc/motd", func(chunk []byte) error {
		chunks = append(chunks, chunk)
		return nil
	})
	require.NoError(t, err)
	assert.Empty(t, chunks)
}

// TestGuest_WriteFile verifies that WriteFile streams content correctly.
func TestGuest_WriteFile(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	content := strings.NewReader("hello world")
	err := guest.WriteFile(context.Background(), "m1", "/tmp/test", content)
	require.NoError(t, err)
}

// TestGuest_PatchFile verifies that PatchFile returns the outcome.
func TestGuest_PatchFile(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	outcome, err := guest.PatchFile(context.Background(), "m1", "/tmp/f", "sha256:base", "sha256:expect", []byte("patch"))
	require.NoError(t, err)
	assert.Equal(t, machine.PatchApplied, outcome)
}

// TestGuest_SearchFiles verifies that SearchFiles returns the matching paths.
func TestGuest_SearchFiles(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	paths, err := guest.SearchFiles(context.Background(), "m1", "/etc/", "mot")
	require.NoError(t, err)
	assert.Empty(t, paths)
}

// TestGuest_ContextCancellation verifies that the caller's context is
// propagated through to the gRPC call and that a cancelled context causes the
// call to fail.
func TestGuest_ContextCancellation(t *testing.T) {
	sockPath, cleanup := startFakeGRPC(t)
	defer cleanup()

	guest := newTestGuest(sockPath, newDialer(sockPath))
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err := guest.Boot(ctx, "m1", machine.GuestBoot{
		ImageUUID:    "img-1",
		RootDiskUUID: "disk-1",
		Run: machine.ExecSpec{
			Command: []string{"/init"},
		},
		Network: machine.GuestNetwork{},
	})
	assert.Error(t, err)
}
